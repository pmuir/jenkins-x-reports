package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

const maxUploadSize = 2 * 1024 // 2 MB
const uploadPath = "/Users/pmuir/tmp/reports"
const downloadPort = 8080
const uploadPort = 8081
const bind = "0.0.0.0"

func main() {
	downloadServer()
	go uploadServer()
}

func downloadServer() {
	server:= http.NewServeMux()
	server.Handle("/", http.FileServer(http.Dir(uploadPath)))
	log.Printf("Download server listening on %s:%d\n", bind, downloadPort)
	http.ListenAndServe(fmt.Sprintf("%s:%d", bind, downloadPort), server)
}

func uploadServer() {
	server:= http.NewServeMux()
	server.HandleFunc("/", uploadFileHandler())
	log.Printf("Upload server listening on %s:%d\n", bind, uploadPort)
	http.ListenAndServe(fmt.Sprintf("%s:%d", bind, uploadPort), server)
}

func uploadFileHandler() http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// validate file size
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
		if err := r.ParseMultipartForm(maxUploadSize); err != nil {
			renderError(w, "FILE_TOO_BIG", http.StatusBadRequest)
			return
		}

		// parse and validate file and post parameters
		file, _, err := r.FormFile("uploadFile")
		if err != nil {
			renderError(w, "INVALID_FILE", http.StatusBadRequest)
			return
		}
		defer file.Close()
		fileBytes, err := ioutil.ReadAll(file)
		if err != nil {
			renderError(w, "INVALID_FILE", http.StatusBadRequest)
			return
		}
		newPath := filepath.Join(uploadPath, r.URL.Path)
		fmt.Printf("File: %s\n", newPath)

		// write file
		newFile, err := os.Create(newPath)
		if err != nil {
			renderError(w, "CANT_WRITE_FILE", http.StatusInternalServerError)
			return
		}
		defer newFile.Close() // idempotent, okay to call twice
		if _, err := newFile.Write(fileBytes); err != nil || newFile.Close() != nil {
			renderError(w, "CANT_WRITE_FILE", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("SUCCESS"))
	})
}

func renderError(w http.ResponseWriter, message string, statusCode int) {
	w.WriteHeader(http.StatusBadRequest)
	w.Write([]byte(message))
}
