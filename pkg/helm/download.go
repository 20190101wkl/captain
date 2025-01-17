package helm

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/alauda/captain/pkg/chartrepo"
	"github.com/alauda/captain/pkg/registry"
	appv1 "github.com/alauda/helm-crds/pkg/apis/app/v1"
	"github.com/go-logr/logr"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/repo"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	ChartsDir = "/tmp/helm-charts"

	transCfg = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // ignore expired SSL certificates
	}
	httpClient = &http.Client{Timeout: 30 * time.Second, Transport: transCfg}

	repoCache = cache.New(5*time.Minute, 10*time.Minute)
)

type Downloader struct {
	incfg *rest.Config
	cfg   *rest.Config

	// system ns
	ns string

	log logr.Logger
}

func NewDownloader(ns string, incfg, cfg *rest.Config, log logr.Logger) *Downloader {
	return &Downloader{
		incfg: incfg,
		cfg:   cfg,
		ns:    ns,
		log:   log,
	}
}

// stable/nginx -> stable nginx
func getRepoAndChart(name string) (string, string) {
	data := strings.Split(name, "/")
	if len(data) != 2 {
		return "", ""
	}
	return data[0], data[1]
}

// get from cache first, then get from k8s
func (d *Downloader) getRepoInfo(name string, ns string) (*repo.Entry, error) {
	result, ok := repoCache.Get(name)
	if ok {
		return result.(*repo.Entry), nil
	}
	entry, err := chartrepo.GetChartRepo(name, ns, d.incfg)
	if err == nil {
		repoCache.SetDefault(name, entry)
	}
	return entry, err
}

// downloadChart download a chart from helm repo to local disk and return the path
// name: <repo>/<chart>
func (d *Downloader) downloadChart(name string, version string) (string, error) {
	log := d.log

	repoName, chart := getRepoAndChart(name)
	if repoName == "" && chart == "" {
		return "", errors.New("cannot parse chart name")
	}
	log.Info("get chart", "name", name, "version", version)

	dir := ChartsDir
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err = os.MkdirAll(dir, 0755); err != nil {
			return "", err
		}
		log.Info("helm charts dir not exist, create it: ", "dir", dir)
	}

	entry, err := d.getRepoInfo(repoName, d.ns)
	if err != nil {
		log.Error(err, "get chartrepo error")
		return "", err
	}

	chartResourceName := fmt.Sprintf("%s.%s", strings.ToLower(chart), repoName)

	cv, err := chartrepo.GetChart(chartResourceName, version, d.ns, d.incfg)
	if err != nil {
		log.Error(err, "get chart error")
		return "", err
	}

	path := cv.URLs[0]

	fileName := strings.Split(path, "/")[1]
	filePath := fmt.Sprintf("%s/%s-%s-%s", dir, repoName, cv.Digest, fileName)

	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		log.Info("chart already downloaded, use it", "path", filePath)
		return filePath, nil
	}

	if err := downloadFileFromEntry(entry, path, filePath); err != nil {
		log.Error(err, "download chart to disk error")
		return "", err
	}

	log.Info("download chart to disk", "path", filePath)

	return filePath, nil

}

// downloadFileFromEntry will download a url and store it in local filepath.
// It writes to the destination file as it downloads it, without
// loading the entire file into memory.
func downloadFileFromEntry(entry *repo.Entry, chartPath, filepath string) error {
	ep := entry.URL + "/" + chartPath
	if strings.HasSuffix(entry.URL, "/") {
		ep = entry.URL + chartPath
	}

	if strings.HasPrefix(chartPath, "http://") || strings.HasPrefix(chartPath, "https://") {
		ep = chartPath
	}

	return downloadFile(ep, entry.Username, entry.Password, filepath)
}

func downloadFile(url, username, password, filepath string) error {
	req, err := http.NewRequest("GET", url, nil)
	if username != "" && password != "" {
		req.SetBasicAuth(username, password)
	}

	// Get the data
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return errors.Errorf("failed to fetch %s : %s", url, resp.Status)
	}

	buf := bytes.NewBuffer(nil)
	_, err = io.Copy(buf, resp.Body)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath, buf.Bytes(), 0644); err != nil {
		return err
	}

	return nil
}

func (d *Downloader) downloadChartFromHTTP(hr *appv1.HelmRequest) (string, error) {
	var filePath string
	var err error
	if hr.Spec.Source != nil && hr.Spec.Source.HTTP != nil {
		if hr.Spec.Source.HTTP.URL != "" {
			dir := ChartsDir
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				if err = os.MkdirAll(dir, 0755); err != nil {
					return "", err
				}
				log.Info("helm charts dir not exist, create it: ", "dir", dir)
			}

			url := hr.Spec.Source.HTTP.URL
			chname := splitChartNameFromURL(url)

			filePath = fmt.Sprintf("%s/%s-%s", dir, hr.GetName(), chname)
			if _, err := os.Stat(filePath); !os.IsNotExist(err) {
				log.Info("chart already downloaded, remove it", "path", filePath)
				os.Remove(filePath)
			}

			username, password := "", ""
			if hr.Spec.Source.HTTP.SecretRef != "" {
				username, password, err = d.fetchAuthFromSecret(hr.Spec.Source.HTTP.SecretRef, hr.GetNamespace())
				if err != nil {
					return "", err
				}
			}

			if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				if err := downloadFile(url, username, password, filePath); err != nil {
					return "", err
				}
				log.Info("successfully download chart from url", "url", url)
			} else {
				err = errors.New("helmrequest spec source http url does not start with HTTP or HTTPS")
			}
		} else {
			err = errors.New("helmrequest spec source http url not found")
		}
	}

	return filePath, err
}

func (d *Downloader) pullOCIChart(hr *appv1.HelmRequest) (*chart.Chart, error) {
	client, err := registry.NewClient(
		registry.ClientOptDebug(true),
	)
	if err != nil {
		return nil, err
	}

	if hr.Spec.Source != nil && hr.Spec.Source.OCI != nil {
		username, password := "", ""
		if hr.Spec.Source.OCI.SecretRef != "" {
			username, password, err = d.fetchAuthFromSecret(hr.Spec.Source.OCI.SecretRef, hr.GetNamespace())
			if err != nil {
				return nil, err
			}
		}
		ref, err := registry.ParseReference(hr.Spec.Source.OCI.Repo)
		if err != nil {
			return nil, err
		}
		if err := client.PullChart(ref, true, true, username, password); err != nil {
			return nil, err
		}

		cht, err := client.LoadChart(ref)
		if err != nil {
			return nil, err
		}

		return cht, nil
	}

	return nil, errors.New("invalid chart Source, need OCI type")
}

func (d *Downloader) fetchAuthFromSecret(name, namespace string) (string, string, error) {
	inkc, err := kubernetes.NewForConfig(d.incfg)
	if err != nil {
		log.Error(err, "init kubernetes client error")
		return "", "", err
	}

	s, err := inkc.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			kc, errI := kubernetes.NewForConfig(d.cfg)
			if errI != nil {
				log.Error(errI, "init incluster kubernetes client error")
				return "", "", errI
			}
			s, err = kc.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				return "", "", err
			}
		} else {
			return "", "", err
		}
	}
	username, password := "", ""

	u, ok := s.Data["username"]
	if ok {
		username = strings.Trim(string(u), "\n")
	}
	p, ok := s.Data["password"]
	if ok {
		password = strings.Trim(string(p), "\n")
	}

	if username == "" || password == "" {
		return "", "", errors.New(fmt.Sprintf("can not find username or password in the secret %s/%s", namespace, name))
	}

	return username, password, nil
}

func splitChartNameFromURL(url string) string {
	if len(url) == 0 {
		return ""
	}

	idx := strings.LastIndex(url, "/")
	if idx == -1 {
		return url
	}
	return url[idx+1:]
}
