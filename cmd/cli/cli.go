package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/xheize/git-updater/internal/gitManager"
)

func main() {
	serverFlag := flag.String("server", "", "Git Updater Server URL (can also be set via GIT_UPDATER_SERVER_URL env, defaults to http://localhost:3000)")
	fileFlag := flag.String("file", "", "Target YAML file path to update (required)")
	imageFlag := flag.String("image", "", "Container image name (required)")
	tagFlag := flag.String("tag", "", "Container image tag (required)")

	flag.Parse()

	serverURL := *serverFlag
	if serverURL == "" {
		serverURL = os.Getenv("GIT_UPDATER_SERVER_URL")
	}
	if serverURL == "" {
		serverURL = "http://localhost:3000"
	}

	// Ensure prefix http:// or https://
	if !strings.HasPrefix(serverURL, "http://") && !strings.HasPrefix(serverURL, "https://") {
		serverURL = "http://" + serverURL
	}

	if *fileFlag == "" {
		fmt.Fprintln(os.Stderr, "Error: -file parameter is required")
		flag.Usage()
		os.Exit(1)
	}

	if *imageFlag == "" {
		fmt.Fprintln(os.Stderr, "Error: -image parameter is required")
		flag.Usage()
		os.Exit(1)
	}

	if *tagFlag == "" {
		fmt.Fprintln(os.Stderr, "Error: -tag parameter is required")
		flag.Usage()
		os.Exit(1)
	}

	// Create job payload
	job := gitManager.Job{
		ID:        fmt.Sprintf("cli-%d", time.Now().UnixNano()),
		File:      *fileFlag,
		Image:     *imageFlag,
		Tag:       *tagFlag,
		Timestamp: time.Now(),
	}

	payloadBytes, err := json.Marshal(job)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to serialize payload: %v\n", err)
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/webhook", strings.TrimSuffix(serverURL, "/"))
	fmt.Printf("Sending update request to: %s\n", url)
	fmt.Printf("Payload: file=%s, image=%s, tag=%s\n", job.File, job.Image, job.Tag)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to create request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to send request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to read response body: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Response Status: %s\n", resp.Status)
	if len(bodyBytes) > 0 {
		fmt.Printf("Response Body: %s\n", string(bodyBytes))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "Error: server returned non-2xx status code: %d\n", resp.StatusCode)
		os.Exit(1)
	}

	fmt.Println("Image update request successfully triggered!")
}
