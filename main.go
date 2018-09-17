package main

import (
	"bytes"
	json2 "encoding/json"
	"errors"
	"fmt"
	"github.com/clbanning/mxj"
	"github.com/clbanning/mxj/x2j"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const maxUploadSize = 2 * 1024 * 1024 // 2 MB
const uploadPath = "/reports"
const downloadPort = 8080
const uploadPort = 8081
const bind = "0.0.0.0"
const url = "http://jenkins-x-reports-elasticsearch-client:9200/tests/junit/"

func main() {
	go uploadServer()
	downloadServer()
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
			log.Println(err)
			renderError(w, "FILE_TOO_BIG", http.StatusBadRequest)
			log.Println(err)
			return
		}

		// parse and validate file and post parameters
		file, _, err := r.FormFile("upload")
		if err != nil {
			renderError(w, "INVALID_FILE", http.StatusBadRequest)
			log.Println(err)
			return
		}
		defer file.Close()
		fileBytes, err := ioutil.ReadAll(file)
		if err != nil {
			renderError(w, "INVALID_FILE", http.StatusBadRequest)
			log.Println(err)
			return
		}
		newPath := filepath.Join(uploadPath, r.URL.Path)

		dir, _ := filepath.Split(newPath)
		err = os.MkdirAll(dir, os.FileMode(0755))
		if err != nil {
			renderError(w, "CANT_CREATE_DIR", http.StatusInternalServerError)
			log.Println(err)
			return
		}
		// write file
		newFile, err := os.Create(newPath)
		if err != nil {
			renderError(w, "CANT_WRITE_FILE", http.StatusInternalServerError)
			log.Println(err)
			return
		}
		defer newFile.Close() // idempotent, okay to call twice
		if _, err := newFile.Write(fileBytes); err != nil || newFile.Close() != nil {
			renderError(w, "CANT_WRITE_FILE", http.StatusInternalServerError)
			log.Println(err)
			return
		}
		h := r.Header.Get("X-Content-Type")
		if h == "text/vnd.junit-xml" {
			err = sendToElasticSearch(r.Body, r.URL.Path)
			if err != nil {
				renderError(w, "CANT_SEND_TO_ELASTICSEATCH", http.StatusInternalServerError)
				log.Println(err)
			}
		}
		w.Write([]byte("SUCCESS"))

	})
}

func sendToElasticSearch(reader io.Reader, path string) error {
	_, json, err := x2j.XmlReaderToJson(reader)
	if err != nil {
		return err
	}
	json, err = toJson(json)
	fmt.Printf("Successfully annnotated JUnit result with build info\n")
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(json))

	req.Header.Set("Content-Type", "application/json")

	if err != nil {
		return err
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if (resp.StatusCode >= 200 && resp.StatusCode < 300 ) {
		fmt.Printf("Sent %s to %s\n", path, url)
	} else {
		body, _ := ioutil.ReadAll(resp.Body)
		return errors.New(fmt.Sprintf("HTTP status: %s; HTTP Body: %s\n", resp.Status, body))
	}
	return nil
}

func toJson(json []byte) ([]byte, error) {
	m, err := mxj.NewMapJson(json)
	if err != nil {
		return nil, err
	}

	if err != nil {
		return nil, err
	}
	// Kibana is quite restrictive in the way it accepts JSON, so just rebuild the JSON entirely!

	utc, _ := time.LoadLocation("UTC")
	data := map[string]interface{} {
		"org": os.Getenv("ORG"),
		"appName": os.Getenv("APP_NAME"),
		"version": os.Getenv("VERSION"),
		"errors": m.ValueOrEmptyForPathString("testsuite.-errors"),
		"failures": m.ValueOrEmptyForPathString("testsuite.-failures"),
		"testsuiteName": m.ValueOrEmptyForPathString("testsuite.-name"),
		"skippedTests": m.ValueOrEmptyForPathString("testsuite.-skipped"),
		"tests": m.ValueOrEmptyForPathString("testsuite.-tests"),
		"time": m.ValueOrEmptyForPathString("testsuite.-time"),
		"timestamp": time.Now().In(utc).Format("2006-01-02T15:04:05Z"),
		// TODO Add the TestCases
	}
	fmt.Printf("%s", data)
	return json2.Marshal(data)
}

func renderError(w http.ResponseWriter, message string, statusCode int) {
	w.WriteHeader(http.StatusBadRequest)
	w.Write([]byte(message))
}
