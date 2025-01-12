package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func mediaTypeToFileExtension(mediaType string) string {
	switch mediaType {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	}
	return ""
}

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
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

	fmt.Println("downloading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error parsing form", err)
		return
	}

	formFile, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer formFile.Close()

	rawThumbnailID := make([]byte, 32)
	_, err = rand.Read(rawThumbnailID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error generating thumbnail ID", err)
		return
	}
	thumbnailID := make([]byte, base64.RawURLEncoding.EncodedLen(len(rawThumbnailID)))
	base64.RawURLEncoding.Encode(thumbnailID, rawThumbnailID)

	mediaType := header.Header.Get("Content-Type")
	fileExt := mediaTypeToFileExtension(mediaType)
	filePath := fmt.Sprintf("%s/%s.%s", cfg.assetsRoot, string(thumbnailID), fileExt)

	assetFile, err := os.Create(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating file", err)
		return
	}

	_, err = io.Copy(assetFile, formFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error copying file", err)
		return
	}

	thumbnailURL := fmt.Sprintf("http://localhost:%s/%s", cfg.port, filePath)
	dbVideo.ThumbnailURL = &thumbnailURL
	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, dbVideo)
}
