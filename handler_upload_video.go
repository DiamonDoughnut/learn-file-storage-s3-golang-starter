package main

import (
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
	"github.com/google/uuid"
)

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

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't find video", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Video does not belong to user", fmt.Errorf("video metadata lists different creator - cannot modify"))
		return
	}

	const maxMemory = 10 << 30
	bodyReader := http.MaxBytesReader(w, r.Body, maxMemory)
	reqBody := make([]byte, 0)
	_, err = bodyReader.Read(reqBody)
	if err != nil {
		fmt.Printf("Failed to read request body: %s", err.Error())
		w.WriteHeader(400)
		return
	}
	
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Cannot read file contents", err)
		return
	}
	defer file.Close()
	
	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Header must include Content-Type", err)
	}
	var media string
	if strings.HasPrefix(mediaType, "video/") {
		media = strings.TrimPrefix(mediaType, "video/")
	}

	if media != "mp4" {
		respondWithError(w, http.StatusUnsupportedMediaType, "Video upload must be .mp4", fmt.Errorf("incorrect filetype upload"))
		return
	}

	tempFile, err := os.CreateTemp("", fmt.Sprintf("tubely-upload.%s", media))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temp file in local storage", err)
		return
	}
	defer os.Remove(fmt.Sprintf("tubely-upload.%s", media))
	defer tempFile.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to read data from video file", err)
		return
	}

	_, err = tempFile.Write(data)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to write to temp file", err)
		return
	}

	tempFile.Seek(0, io.SeekStart)

	ratio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "getVideoAspectRatio failed", err)
		return
	}

	if ratio == "16:9" {
		ratio = "landscape"
	}
	if ratio == "9:16" {
		ratio = "portrait"
	}

	fastVideoPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusTeapot, "processVideoForFastStart failed", err)
		return
	}

	fastVideo, err := os.Open(fastVideoPath)
	if err != nil {
		respondWithError(w, http.StatusTeapot, "Failed to process video to new file", err)
		return
	}
	defer fastVideo.Close()
	defer os.Remove(fastVideoPath)

	bucketName := os.Getenv("S3_BUCKET")
	nameStr := make([]byte, 32)
	rand.Read(nameStr)
	name := base64.RawURLEncoding.EncodeToString(nameStr)
	name = fmt.Sprintf("%s/%s.%s",ratio, name, media)
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{Bucket: &bucketName, Key: &name, Body: fastVideo, ContentType: &mediaType})
	if err != nil {
		respondWithError(w, http.StatusExpectationFailed, "Failed to create object in remote bucket", err)
		return
	}
	
	distro := os.Getenv("S3_CF_DISTRO")
	distroUrl := fmt.Sprintf("%s/%s", distro, name)
	video.VideoURL = &distroUrl
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video url in database", err)
		return
	}
	
	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	output, err := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath).Output()
	if err != nil {
		return "", err
	}
	type jsonOut struct {
		Streams		[]struct {
			Width	int `json:"width"`
			Height  int `json:"height"`
		} `json:"streams"`
	}
	var out jsonOut
	err = json.Unmarshal(output, &out)
	if err != nil {
		return "", fmt.Errorf("no video streams found")
	}
	height := out.Streams[0].Height
	width := out.Streams[0].Width
	ratio := float64(width) / float64(height)
	if math.Abs(ratio - 16.0/9.0) < 0.01 {
		return "16:9", nil
	}
	if math.Abs(ratio - 9.0/16.0) < 0.01 {
		return "9:16", nil
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outputPath := fmt.Sprintf("%s.processing", filePath)
	ffmpgCmd := exec.Command("ffmpeg", "-y", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputPath)
	output, err := ffmpgCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("error running ffmpeg: %s, output %s", err.Error(), string(output))
	}
	return outputPath, nil
}

func generatePresignedUrl(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignedClient := s3.NewPresignClient(s3Client)
	presignedRequest, err := presignedClient.PresignGetObject(context.Background(), &s3.GetObjectInput{Bucket: &bucket, Key: &key}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to get presigned url: %s", err.Error())
	}
	return presignedRequest.URL, nil
}

