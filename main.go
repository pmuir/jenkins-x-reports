package main

import (
	"bytes"
	json2 "encoding/json"
	"errors"
	"fmt"
	"github.com/clbanning/mxj"
	"github.com/clbanning/mxj/x2j"
	jenkinsxv1 "github.com/jenkins-x/jx/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx/pkg/client/clientset/versioned"
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
var kubernetesClient kubernetes.Interface
var jenkinsClient versioned.Interface

func main() {
	var err error

	kubernetesClient, err = createKubernetesClient()
	if err != nil {
		panic(err)
	}

	jenkinsClient, err = createJenkinsClient()

	if err != nil {
		panic(err)
	}
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

		// Get and validate headers
		org := r.Header.Get("X-Org")
		if org == "" {
			renderError(w, "MUST_PROVIDE_X-ORG_HEADER", http.StatusInternalServerError)
			log.Println("No X-Org HEADER provided")
		}
		app := r.Header.Get("X-App")
		if app == "" {
			renderError(w, "MUST_PROVIDE_X-APP_HEADER", http.StatusInternalServerError)
			log.Println("No X-App HEADER provided")
		}
		version := r.Header.Get("X-Version")
		if version == "" {
			renderError(w, "MUST_PROVIDE_X-VERSION_HEADER", http.StatusInternalServerError)
			log.Println("No X-Version HEADER provided")
		}
		buildNo := r.Header.Get("X-Build-Number")
		if buildNo == "" {
			renderError(w, "MUST_PROVIDE_X-BUILD-NUMBER_HEADER", http.StatusInternalServerError)
			log.Println("No X-Build-Number provided")
		}
		branch := r.Header.Get("X-Branch")
		if branch == "" {
			renderError(w, "MUST_PROVIDE_X-BRANCH_HEADER", http.StatusInternalServerError)
			log.Println("No X-Branch provided")
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
		dir := filepath.Join(uploadPath, org, app, version)
		newPath := filepath.Join(dir, filename)

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
		cm, err := getOrCreateConfigMap(org, app)
		if err != nil {
			renderError(w, "ERROR_CREATING_CONFIG_MAP", http.StatusInternalServerError)
			log.Println(err)
		}
		reportHost, err := getReportHost()
		if err != nil {
			renderError(w, "ERROR_CREATING_CONFIG_MAP", http.StatusInternalServerError)
			log.Println(err)
		}

		url := fmt.Sprintf("%s/%s/%s/%s/%s", reportHost, org, app, version, filename)
		cm, err = updateConfigMap(cm, version, filename, url )
		if err != nil {
			renderError(w, "ERROR_UPDATING_CONFIG_MAP", http.StatusInternalServerError)
			log.Println(err)
		}
		_, err = updatePipelineActivity(buildNo, branch, org, app, version, filename, url )
		if err != nil {
			renderError(w, "ERROR_UPDATING_PIPELINE_ACTIVITY", http.StatusInternalServerError)
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
		"org": org,
		"appName": appName,
		"version": version,
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

func createJenkinsClient() (versioned.Interface, error) {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	// creates the client
	client, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func getOrCreateConfigMap(org string, app string) (*corev1.ConfigMap, error) {
	cmName := fmt.Sprintf("%s-%s-test-reports", org, app)
	cm, err := kubernetesClient.CoreV1().ConfigMaps(cmNamespace).Get(cmName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if cm == nil {
		return kubernetesClient.CoreV1().ConfigMaps(cmNamespace).Create(&corev1.ConfigMap{
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

func updateConfigMap(cm *corev1.ConfigMap, version string, filename string, url string) (*corev1.ConfigMap, error){
	if cm.Data[version] == "" {
		cm.Data[version] = fmt.Sprintf("|-\n")
	}
	cm.Data[version] = fmt.Sprintf("%s\n    %s: %s\n", cm.Data[version], filename, url)
	return kubernetesClient.CoreV1().ConfigMaps(cmNamespace).Update(cm)
}

func getReportHost() (string, error) {
	svc, err := kubernetesClient.CoreV1().Services("jx-production").Get("jenkins-x-reports", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return svc.Annotations["fabric8.io/exposeUrl"], nil
}

func updatePipelineActivity(buildNo string, branch string, org string, app string, version string, filename string, url string) (*jenkinsxv1.PipelineActivity, error) {
	pa, err :=jenkinsClient.JenkinsV1().PipelineActivities(cmNamespace).Get(fmt.Sprintf("%s-%s-%s-%s", org, app, branch, buildNo), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	annotationName := "jenkins-x-reports"
	if pa.Annotations == nil {
		pa.Annotations = map[string]string {}
	}
	pa.Annotations[annotationName] = fmt.Sprintf("%s- %s: %s\n", pa.Annotations[annotationName], filename, url)
	return jenkinsClient.JenkinsV1().PipelineActivities(cmNamespace).Update(pa)
}
