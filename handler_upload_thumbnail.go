package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

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


	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20
	r.ParseMultipartForm(maxMemory)

	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		fmt.Printf("Error parsing form file: %s", err.Error())
	    w.WriteHeader(500)
		return
	}
	defer file.Close()
	mediaHeader := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(mediaHeader)
	if err != nil {
		fmt.Printf("Invalid header structure: %s", err.Error())
		w.WriteHeader(400)
		return
	}
	if mediaType != "image/png" && mediaType != "image/jpeg" {
		respondWithError(w, 415, "thumbnails must be .png or .jpg files only", fmt.Errorf("invalid media type"))
		return
	}
	data, err := io.ReadAll(file)
	if err != nil {
		fmt.Printf("Error reading file: %s", err.Error())
		w.WriteHeader(500)
		return
	}
	metadata, err := cfg.db.GetVideo(videoID)
	if err != nil {
		fmt.Printf("Error getting video data: %s", err.Error())
		w.WriteHeader(404)
		return
	}
	if metadata.UserID != userID {
		fmt.Printf("UserID does not match Video owner")
		w.WriteHeader(401)
		return
	}
	var newThumbnail thumbnail
	newThumbnail.data = data
	newThumbnail.mediaType = mediaType
	url := make([]byte, 32)
	_, err = rand.Read(url)
	if err != nil {
		fmt.Printf("Failed to create random url seed")
	}
	imageUrl := base64.RawURLEncoding.EncodeToString([]byte(url))
	if strings.Contains(mediaType, "image/") {
		mediaType = strings.TrimPrefix(mediaType, "image/")
		if mediaType == "jpeg" {
			mediaType = "jpg"
		}
	}
	filePath := filepath.Join(cfg.assetsRoot, fmt.Sprintf("%s.%s", imageUrl, mediaType))
	newFile, err := os.Create(filePath)
	if err != nil {
		fmt.Printf("Could not create new file in storage: %s", err.Error())
		w.WriteHeader(500)
		return
	}
	_, err = newFile.Write(data)
	if err != nil {
		fmt.Printf("Could not copy file to storage: %s", err.Error())
	}
	newFile.Close()
	
	filePath = fmt.Sprintf("http://localhost:%s/assets/%s.%s", cfg.port, imageUrl, mediaType)
	metadata.ThumbnailURL = &filePath
	err = cfg.db.UpdateVideo(metadata)
	if err != nil {
		fmt.Printf("Error updating video thumbnail: %s", err.Error())
	}

	respondWithJSON(w, http.StatusOK, metadata)
}
