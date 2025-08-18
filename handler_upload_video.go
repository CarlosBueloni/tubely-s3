package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const upload_limit = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, upload_limit)
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
		respondWithError(w, http.StatusInternalServerError, "Error getting the video", err)
		return
	}
	if userID != video.UserID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized access, video does not belong to user", err)
		return
	}

	err = r.ParseMultipartForm(upload_limit)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "File is too large", err)
		return
	}

	fildeData, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "File error", err)
		return
	}

	defer fildeData.Close()

	contentType := header.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	fmt.Printf("mediatype %v", mediaType)
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusUnsupportedMediaType, "Unsupported media type", err)
		return
	}

	temp, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temporary file", err)
		return
	}
	defer os.Remove(temp.Name())
	defer temp.Close()

	if _, err := io.Copy(temp, fildeData); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Temp file error", err)
		return
	}

	temp.Seek(0, io.SeekStart)

	k := make([]byte, 32)
	rand.Read(k)
	randString := base64.URLEncoding.EncodeToString(k)
	fileKey := randString + ".mp4"
	params := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        temp,
		ContentType: &mediaType,
	}

	_, err = cfg.s3Client.PutObject(context.Background(), &params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error generating s3 object", err)
		return
	}

	videoURL := fmt.Sprintf("https://%v.s3.%v.amazonaws.com/%v", cfg.s3Bucket, cfg.s3Region, fileKey)
	video.VideoURL = &videoURL
	cfg.db.UpdateVideo(video)
}
