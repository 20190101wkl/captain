package helm

import (
	"bytes"
	"fmt"
	"github.com/alauda/captain/pkg/chartrepo"
	"github.com/go-logr/logr"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	"helm.sh/helm/pkg/repo"
	"io"
	"io/ioutil"
	"k8s.io/client-go/rest"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	ChartsDir = "/tmp/helm-charts"
)

var repoCache = cache.New(5*time.Minute, 10*time.Minute)

type Downloader struct {
	cfg *rest.Config

	// system ns
	ns string

	log logr.Logger
}

func NewDownloader(ns string, cfg *rest.Config, log logr.Logger) *Downloader {
	return &Downloader{
		ns:  ns,
		cfg: cfg,
		log: log,
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
	entry, err := chartrepo.GetChartRepo(name, ns, d.cfg)
	if err == nil {
		repoCache.SetDefault(name, entry)
	}
	return entry, err
}

// cal the local path for a chart
// name: <repo>/<name>
func getChartPath(name, version string) string {
	new := strings.Replace(name, "/", "-", -1)
	return fmt.Sprintf("%s/%s-%s.tgz", ChartsDir, new, version)
}

func (d *Downloader) downloadChart(name string, version string) (string, error) {
	log := d.log

	repo, chart := getRepoAndChart(name)
	if repo == "" && chart == "" {
		return "", errors.New("cannot parse chart name")
	}

	log.Info("get chart", "name", name, "version", version)

	dir := ChartsDir

	if version != "" {
		// we can check the path now
		checkPath := getChartPath(name, version)
		if _, err := os.Stat(checkPath); !os.IsNotExist(err) {
			log.Info("chart already downloaded, use it", "path", checkPath)
			return checkPath, nil
		}
	}

	entry, err := d.getRepoInfo(repo, d.ns)
	if err != nil {
		log.Error(err, "get chartrepo error")
		return "", err
	}

	chartResourceName := fmt.Sprintf("%s.%s", strings.ToLower(chart), repo)

	path, err := chartrepo.GetChart(chartResourceName, version, d.ns, d.cfg)
	if err != nil {
		log.Error(err, "get chart error")
		return "", err
	}

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err = os.MkdirAll(dir, 0755); err != nil {
			return "", err
		}
		log.Info("dir not exist, create it: ", "dir", dir)
	}

	fileName := strings.Split(path, "/")[1]
	filePath := fmt.Sprintf("%s/%s-%s", dir, repo, fileName)

	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		log.Info("chart already downloaded, use it", "path", filePath)
		return filePath, nil
	}

	if err := downloadFile(entry, path, filePath); err != nil {
		log.Error(err, "download chart to disk error")
		return "", err
	}

	return filePath, nil

}

// downloadFile will download a url and store it in local filepath.
// It writes to the destination file as it downloads it, without
// loading the entire file into memory.
func downloadFile(entry *repo.Entry, chartPath, filepath string) error {

	client := &http.Client{Timeout: 30 * time.Second}
	url := entry.URL + "/" + chartPath
	if strings.HasSuffix(entry.URL, "/") {
		url = entry.URL + chartPath
	}
	req, err := http.NewRequest("GET", url, nil)
	if entry.Username != "" && entry.Password != "" {
		req.SetBasicAuth(entry.Username, entry.Password)
	}

	// Get the data
	resp, err := client.Do(req)
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