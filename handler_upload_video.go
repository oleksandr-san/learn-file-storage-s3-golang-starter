package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func mediaTypeToVideoExtension(mediaType string) string {
	switch mediaType {
	case "video/mp4":
		return "mp4"
	case "video/quicktime":
		return "mov"
	default:
		return ""
	}
}

const float64EqualityThreshold = 1e-3

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) <= float64EqualityThreshold
}

func getVideoAspectRatio(filePath string) (string, error) {
	type ffprobeOutput struct {
		Streams []struct {
			Width              int    `json:"width"`
			Height             int    `json:"height"`
			DisplayAspectRatio string `json:"display_aspect_ratio"`
		} `json:"streams"`
	}

	var out bytes.Buffer
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	cmd.Stdout = &out

	err := cmd.Run()
	if err != nil {
		return "", err
	}

	fmt.Println(out.String())
	var outData ffprobeOutput
	err = json.Unmarshal(out.Bytes(), &outData)
	if err != nil {
		return "", err
	}

	if len(outData.Streams) == 0 {
		return "", fmt.Errorf("no streams found")
	}

	for _, stream := range outData.Streams {
		if stream.DisplayAspectRatio != "" {
			return stream.DisplayAspectRatio, nil
		}
	}

	width := outData.Streams[0].Width
	height := outData.Streams[0].Height
	aspectRatio := float64(width) / float64(height)
	if almostEqual(aspectRatio, 16.0/9.0) {
		return "16:9", nil
	} else if almostEqual(aspectRatio, 9.0/16.0) {
		return "9:16", nil
	} else {
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return outputFilePath, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	dbVideo, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Error getting video", err)
		return
	}

	if dbVideo.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "User does not own video", nil)
		return
	}

	fmt.Println("downloading video", videoID, "by user", userID)

	const maxMemory = 10 << 30
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error parsing form", err)
		return
	}

	formFile, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer formFile.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error parsing media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "video-upload-*."+mediaTypeToVideoExtension(mediaType))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, formFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error copying file to temp", err)
		return
	}

	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting video aspect ratio", err)
		return
	}
	videoPrefix := "other"
	switch aspectRatio {
	case "16:9":
		videoPrefix = "landscape"
	case "9:16":
		videoPrefix = "portrait"
	}

	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video for fast start", err)
		return
	}
	defer os.Remove(processedFilePath)
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed file", err)
		return
	}
	defer processedFile.Close()

	rawVideoID := make([]byte, 32)
	_, err = rand.Read(rawVideoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error generating thumbnail ID", err)
		return
	}
	s3VideoID := make([]byte, base64.RawURLEncoding.EncodedLen(len(rawVideoID)))
	base64.RawURLEncoding.Encode(s3VideoID, rawVideoID)

	s3VideoKey := fmt.Sprintf("%s/%s.%s", videoPrefix, s3VideoID, mediaTypeToVideoExtension(mediaType))
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3VideoKey,
		Body:        processedFile,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading video to S3", err)
		return
	}

	videoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, s3VideoKey)
	dbVideo.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video", err)
		return
	}

	dbVideo, err = cfg.dbVideoToSignedVideo(r.Context(), dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error signing video URL", err)
		return
	}
	respondWithJSON(w, http.StatusOK, dbVideo)
}

func (cfg *apiConfig) dbVideoToSignedVideo(
	ctx context.Context,
	video database.Video,
) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	components := strings.Split(*video.VideoURL, ",")
	if len(components) < 2 {
		return video, nil
	}

	bucket := components[0]
	key := components[1]
	signedURL, err := generatePresignedURL(ctx, cfg.s3Client, bucket, key, 15*time.Minute)
	if err != nil {
		return video, err
	}

	video.VideoURL = &signedURL
	return video, nil
}

func generatePresignedURL(
	ctx context.Context,
	s3Client *s3.Client,
	bucket, key string,
	expireTime time.Duration,
) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	req, err := presignClient.PresignGetObject(
		ctx,
		&s3.GetObjectInput{
			Bucket: &bucket,
			Key:    &key,
		},
		s3.WithPresignExpires(expireTime),
	)
	if err != nil {
		return "", err
	}

	url := req.URL
	return url, nil
}
