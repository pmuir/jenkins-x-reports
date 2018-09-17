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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
const url = "http://jenkins-x-reports-elasticsearch-client.jx:9200/tests/junit/"
const cmNamespace = "jx"

func main() {
	var client kubernetes.Interface
	client, err := createKubernetesClient()
	if err != nil {
		panic(err)
	}
	go uploadServer(client)
	downloadServer()
}

func downloadServer() {
	server:= http.NewServeMux()
	server.Handle("/", http.FileServer(http.Dir(uploadPath)))
	log.Printf("Download server listening on %s:%d\n", bind, downloadPort)
	http.ListenAndServe(fmt.Sprintf("%s:%d", bind, downloadPort), server)
}

func uploadServer(client kubernetes.Interface) {
	server:= http.NewServeMux()
	server.HandleFunc("/", uploadFileHandler(client))
	log.Printf("Upload server listening on %s:%d\n", bind, uploadPort)
	http.ListenAndServe(fmt.Sprintf("%s:%d", bind, uploadPort), server)
}

func uploadFileHandler(client kubernetes.Interface) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		// Get and validate headers
		org := r.Header.Get("X-Org")
		if org == "" {
			renderError(w, "MUST_PROVIDE_X-ORG_HEADER", http.StatusInternalServerError)
			log.Println("No X-ORG HEADER provided")
		}
		app := r.Header.Get("X-App")
		if app == "" {
			renderError(w, "MUST_PROVIDE_X-APP_HEADER", http.StatusInternalServerError)
			log.Println("No X-APP HEADER provided")
		}
		version := r.Header.Get("X-Version")
		if version == "" {
			renderError(w, "MUST_PROVIDE_X-VERSION_HEADER", http.StatusInternalServerError)
			log.Println("No X-VERSION HEADER provided")
		}

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
		_, filename := filepath.Split(r.URL.Path)
		newDir := filepath.Join(uploadPath, org, app, version)
		newPath := filepath.Join(newDir, filename)

		err = os.MkdirAll( newDir, os.FileMode(0755))
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
		if r.Header.Get("X-Content-Type") == "text/vnd.junit-xml" {
			reader, err := os.Open(newPath)
			if err != nil {
				renderError(w, "CANT_READ_FILE", http.StatusInternalServerError)
				log.Println(err)
			}
			err = sendToElasticSearch(reader, org, app, version, r.URL.Path)
			if err != nil {
				renderError(w, "CANT_SEND_TO_ELASTICSEATCH", http.StatusInternalServerError)
				log.Println(err)
			}
		}
		cm, err := getOrCreateConfigMap(org, app, client)
		if err != nil {
			renderError(w, "ERROR_CREATING_CONFIG_MAP", http.StatusInternalServerError)
			log.Println(err)
		}
		reportHost, err := getReportHost(client)
		if err != nil {
			renderError(w, "ERROR_CREATING_CONFIG_MAP", http.StatusInternalServerError)
			log.Println(err)
		}

		url := fmt.Sprintf("%s/%s/%s/%s/%s", reportHost, org, app, version, filename)
		cm, err = updateConfigMap(cm, version, filename, url, client )
		if err != nil {
			renderError(w, "ERROR_UPDATING_CONFIG_MAP", http.StatusInternalServerError)
			log.Println(err)
		}
		w.Write([]byte("SUCCESS"))

	})
}

func sendToElasticSearch(reader io.Reader, org string, appName string, version string, path string) error {
	_, json, err := x2j.XmlReaderToJson(reader)
	if err != nil {
		return err
	}
	json, err = toJson(json, org, appName, version)
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

func toJson(json []byte, org string, appName string, version string) ([]byte, error) {
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
		"org": os.Getenv(org),
		"appName": os.Getenv(appName),
		"version": os.Getenv(version),
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

func createKubernetesClient() (kubernetes.Interface, error) {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	// creates the client
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func getOrCreateConfigMap(org string, app string, client kubernetes.Interface) (*corev1.ConfigMap, error) {
	cmName := fmt.Sprintf("%s-%s-test-reports", org, app)
	cm, err := client.CoreV1().ConfigMaps(cmNamespace).Get(cmName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if cm == nil {
		return client.CoreV1().ConfigMaps(cmNamespace).Create(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: cmName,
			},
		})
		if err != nil {
			return nil, err
		}
	}
	return cm, nil
}

func updateConfigMap(cm *corev1.ConfigMap, version string, filename string, url string, client kubernetes.Interface) (*corev1.ConfigMap, error){
	fmt.Printf("Updating %s with data for %s and Data %s\n", cm.Name, version, cm.Data )
	if cm.Data[version] == "" {
		cm.Data[version] = fmt.Sprintf("|-\n")
	}
	cm.Data[version] = fmt.Sprintf("%s\n    %s: %s\n", cm.Data[version], filename, url)
	return client.CoreV1().ConfigMaps(cmNamespace).Update(cm)
}

func getReportHost(client kubernetes.Interface) (string, error) {
	svc, err := client.CoreV1().Services("jx-production").Get("jenkins-x-reports", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return svc.Annotations["fabric8.io/exposeUrl"], nil
}
