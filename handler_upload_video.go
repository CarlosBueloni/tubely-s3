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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
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
	aspectRatio, err := getVideoAspectRatio(temp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting aspect ratio", err)
		return
	}
	processedVideoPath, err := processVideoForFastStart(temp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing file", err)
		return
	}

	processedFile, err := os.Open(processedVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error reading file", err)
		return
	}
	defer processedFile.Close()

	fileKey := aspectRatio + "/" + randString + ".mp4"
	params := s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(fileKey),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	}

	_, err = cfg.s3Client.PutObject(context.Background(), &params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error generating s3 object", err)
		return
	}

	videoURL := fmt.Sprintf("%v,%v", cfg.s3Bucket, fileKey)
	video.VideoURL = &videoURL
	cfg.db.UpdateVideo(video)
}

func getVideoAspectRatio(filepath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filepath)
	var buffer bytes.Buffer
	cmd.Stdout = &buffer
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	var dimensions struct {
		Streams []struct {
			Width, Height int
		} `json:"streams"`
	}
	err = json.Unmarshal(buffer.Bytes(), &dimensions)
	if err != nil {
		return "", err
	}

	if isWithinTolerance(float64(dimensions.Streams[0].Width)/float64(dimensions.Streams[0].Height), 1.77, 0.2) {
		return "landscape", nil
	}

	if isWithinTolerance(float64(dimensions.Streams[0].Width)/float64(dimensions.Streams[0].Height), 0.55, 0.2) {
		return "portrait", nil
	}

	return "other", nil
}

func isWithinTolerance(value, target, tolerance float64) bool {
	return math.Abs(value-target) <= tolerance
}

func processVideoForFastStart(filePath string) (string, error) {
	output := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", output)
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	fmt.Printf("\noutput: %v", output)
	return output, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) < 2 {
		return video, nil
	}
	bucket := parts[0]
	key := parts[1]
	presigned, err := generatePressignedURL(cfg.s3Client, bucket, key, 5*time.Minute)
	if err != nil {
		return video, err
	}
	video.VideoURL = &presigned
	return video, nil
}

func generatePressignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	presignUrl, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL")
	}

	return presignUrl.URL, nil
}
