package main

import (
	"bytes"
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

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

type FFProbeVideo struct {
	Streams []videoStream `json:"streams"`
}

type videoStream struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const max_upload_size = 1 << 30 // 1 GB

	err := r.ParseMultipartForm(max_upload_size)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse multipart form", err)
		return
	}

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
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video from database", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You don't have permission to upload a video for this video ID", nil)
		return
	}

	data, fileHeaders, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video file", err)
		return
	}
	defer data.Close()

	mediaType, _, err := mime.ParseMediaType(fileHeaders.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Unsupported media type. Only MP4 and WebM are allowed", nil)
		return
	}
	
	file, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temporary file", err)
		return
	}

	defer os.Remove(file.Name())
	defer file.Close()

	io.Copy(file, data)

	file.Seek(0, io.SeekStart)

	randVals := make([]byte, 32)

	rand.Read(randVals)

	randChars := base64.RawURLEncoding.EncodeToString(randVals)

	aspectRatio, err := getVideoAspectRatio(file.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}

	videoKey := aspectRatio + randChars

	processedFilePath, err := processVideoForFastStart(file.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for fast start", err)
		return
	}
	
	defer os.Remove(processedFilePath)
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video file", err)
		return
	}
	defer processedFile.Close()


	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &videoKey,
		Body:        processedFile,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video to S3", err)
		return
	}

	videoURL := "https://" + cfg.s3CfDistribution + "/" + videoKey
	video.VideoURL = &videoURL

	cfg.db.UpdateVideo(video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	buffer := &bytes.Buffer{}
	errBuffer := &bytes.Buffer{}

	cmd.Stdout = buffer
	cmd.Stderr = errBuffer

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffprobe error: %v, stderr: %s", err, errBuffer.String())
	}

	var output FFProbeVideo

	err = json.Unmarshal(buffer.Bytes(), &output)
	if err != nil {
		return "", err
	}

	if len(output.Streams) == 0 {
		return "", fmt.Errorf("no video streams found")
	}

	width := output.Streams[0].Width
	height := output.Streams[0].Height

	aspectRatio := float64(width) / float64(height)

	if areFloatsSimilar(aspectRatio, 16.0/9.0) {
		return "landscape/", nil
	} else if areFloatsSimilar(aspectRatio, 9.0/16.0) {
		return "portrait/", nil
	} else {
		return "other/", nil
	}
}

func areFloatsSimilar(a, b float64) bool {
	// If they are exactly equal (e.g., comparing 2.0 and 2.0), return true immediately.
	if a == b {
		return true
	}
	
	// Check if the absolute difference is less than or equal to tolerance.
	return math.Abs(a-b) <= 0.1
}

func processVideoForFastStart(filePath string) (string, error) {
	newOutputPath := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", newOutputPath)

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffmpeg error: %v", err)
	}

	return newOutputPath, nil
}