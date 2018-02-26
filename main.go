package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	yaml "gopkg.in/yaml.v1"

	log "github.com/sirupsen/logrus"

	"github.com/jessevdk/go-flags"
)

var version = "undefined"
var transport *http.Transport

type Config struct {
	Version       bool          `short:"V" long:"version" description:"Display version."`
	PuppetDBURL   string        `short:"u" long:"puppetdb-url" description:"PuppetDB base URL." env:"PROMETHEUS_PUPPETDB_URL" default:"http://puppetdb:8080"`
	CertFile      string        `short:"x" long:"cert-file" description:"A PEM encoded certificate file." env:"PROMETHEUS_CERT_FILE" default:"certs/client.pem"`
	KeyFile       string        `short:"y" long:"key-file" description:"A PEM encoded private key file." env:"PROMETHEUS_KEY_FILE" default:"certs/client.key"`
	CACertFile    string        `short:"z" long:"cacert-file" description:"A PEM encoded CA's certificate file." env:"PROMETHEUS_CACERT_FILE" default:"certs/cacert.pem"`
	SSLSkipVerify bool          `short:"k" long:"ssl-skip-verify" description:"Skip SSL verification." env:"PROMETHEUS_SSL_SKIP_VERIFY"`
	Query         string        `short:"q" long:"puppetdb-query" description:"PuppetDB query." env:"PROMETHEUS_PUPPETDB_QUERY" default:"facts[certname, value] { name='prometheus_exporters' and nodes { deactivated is null } }"`
	Output        string        `short:"o" long:"output" description:"Output. One of stdout, file or configmap" env:"PROMETHEUS_PUPPETDB_OUTPUT" default:"stdout"`
	File          string        `short:"f" long:"config-file" description:"Prometheus target file." env:"PROMETHEUS_PUPPETDB_FILE" default:"/etc/prometheus/targets/prometheus-puppetdb/targets.yml"`
	ConfigMap     string        `long:"configmap" description:"Kubernetes ConfigMap to update." env:"PROMETHEUS_PUPPETDB_CONFIGMAP" default:"prometheus-puppetdb"`
	NameSpace     string        `long:"namespace" description:"Kubernetes NameSpace to use." env:"PROMETHEUS_PUPPETDB_NAMESPACE" default:"default"`
	Sleep         time.Duration `short:"s" long:"sleep" description:"Sleep time between queries." env:"PROMETHEUS_PUPPETDB_SLEEP" default:"5s"`
	Manpage       bool          `short:"m" long:"manpage" description:"Output manpage."`
}

type Node struct {
	Certname  string            `json:"certname"`
	Exporters map[string]string `json:"value"`
}

type StaticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels"`
}

type FileSdConfig struct {
	Files []string `yaml:"files,omitempty"`
}

type RelabelConfig struct {
	SourceLabels []string `yaml:"source_labels,omitempty"`
	Regex        string   `yaml:"regex,omitempty"`
	Action       string   `yaml:"action,omitempty"`
	TargetLabel  string   `yaml:"target_label,omitempty"`
	Replacement  string   `yaml:"replacement,omitempty"`
}

type ScrapeConfig struct {
	JobName        string          `yaml:"job_name,omitempty"`
	FileSdConfigs  []FileSdConfig  `yaml:"file_sd_configs,omitempty"`
	RelabelConfigs []RelabelConfig `yaml:"relabel_configs,omitempty"`
}

type PrometheusConfig struct {
	ScrapeConfigs []ScrapeConfig `yaml:"scrape_configs,omitempty"`
}

func loadConfig(version string) (c Config, err error) {
	parser := flags.NewParser(&c, flags.Default)
	_, err = parser.Parse()
	if err != nil {
		log.Fatal(err)
	}

	if c.Version {
		fmt.Printf("Prometheus-puppetdb v%v\n", version)
		os.Exit(0)
	}

	if c.Manpage {
		var buf bytes.Buffer
		parser.ShortDescription = "Prometheus scrape lists based on PuppetDB"
		parser.WriteManPage(&buf)
		fmt.Printf(buf.String())
		os.Exit(0)
	}
	return
}

func getNodes(client *http.Client, puppetdb string, query string) (nodes []Node, err error) {
	form := strings.NewReader(fmt.Sprintf("{\"query\":\"%s\"}", query))
	puppetdbURL := fmt.Sprintf("%s/pdb/query/v4", puppetdb)
	req, err := http.NewRequest("POST", puppetdbURL, form)
	if err != nil {
		return
	}
	req.Header.Add("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	err = json.Unmarshal(body, &nodes)
	return
}

func getTargets() (c []byte, err error) {
	fileSdConfig := []StaticConfig{}

	nodes, err := getNodes(client, cfg.PuppetDBURL, cfg.Query)
	if err != nil {
		log.Errorf("failed to get nodes: %v", err)
		return
	}

	for _, node := range nodes {
		for jobName, target := range node.Exporters {
			url, err := url.Parse(target)
			if err != nil {
				return nil, err
			}
			staticConfig := StaticConfig{
				Targets: []string{url.Host},
				Labels: map[string]string{
					"certname":     node.Certname,
					"host":         node.Certname,
					"metrics_path": url.Path,
					"job":          jobName,
					"scheme":       url.Scheme,
				},
			}
			fileSdConfig = append(fileSdConfig, staticConfig)
		}
	}
	c, err = yaml.Marshal(&fileSdConfig)
	if err != nil {
		return
	}

	return
}

var client *http.Client
var cfg Config

func init() {
	var err error

	cfg, err = loadConfig(version)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	puppetdbURL, err := url.Parse(cfg.PuppetDBURL)
	if err != nil {
		log.Fatalf("failed to parse PuppetDB URL: %v", err)
	}

	if puppetdbURL.Scheme != "http" && puppetdbURL.Scheme != "https" {
		log.Fatalf("%s is not a valid http scheme\n", puppetdbURL.Scheme)
	}

	if puppetdbURL.Scheme == "https" {
		// Load client cert
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			log.Fatal(err)
		}

		// Load CA cert
		caCert, err := ioutil.ReadFile(cfg.CACertFile)
		if err != nil {
			log.Fatal(err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)

		// Setup HTTPS client
		tlsConfig := &tls.Config{
			Certificates:       []tls.Certificate{cert},
			RootCAs:            caCertPool,
			InsecureSkipVerify: cfg.SSLSkipVerify,
		}
		tlsConfig.BuildNameToCertificate()
		transport = &http.Transport{TLSClientConfig: tlsConfig}
	} else {
		transport = &http.Transport{}
	}

	client = &http.Client{Transport: transport}
}

func main() {
	if cfg.Output == "stdout" {
		c, err := getTargets()
		if err != nil {
			log.Fatalf("failed to get exporters: %v", err)
		}
		fmt.Printf(string(c))
	}
	if cfg.Output == "file" {
		os.MkdirAll(filepath.Dir(cfg.File), 0755)
		for {
			c, err := getTargets()
			if err != nil {
				log.Errorf("failed to get exporters: %v", err)
				break
			}

			err = ioutil.WriteFile(cfg.File, c, 0644)
			if err != nil {
				return
			}

			log.Infof("Sleeping for %v", cfg.Sleep)
			time.Sleep(cfg.Sleep)
		}
	}
	if cfg.Output == "configmap" {
		// creates the in-cluster config
		config, err := rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
		// creates the clientset
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			panic(err.Error())
		}

		configMap, err := clientset.CoreV1().ConfigMaps(cfg.NameSpace).Get(cfg.ConfigMap, metav1.GetOptions{})
		if err != nil {
			configMap = &v1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					Kind:       "ConfigMap",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: cfg.ConfigMap,
				},
				Data: map[string]string{
					"targets.yml": "",
				},
			}
			configMap, err = clientset.CoreV1().ConfigMaps(cfg.NameSpace).Create(configMap)
			if err != nil {
				log.Fatalf("Unable to create ConfigMap: %v", err)
			}
		}

		for {
			c, err := getTargets()
			if err != nil {
				log.Errorf("failed to get exporters: %v", err)
				break
			}

			configMap := &v1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					Kind:       "ConfigMap",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: cfg.ConfigMap,
				},
				Data: map[string]string{
					"targets.yml": string(c),
				},
			}
			configMap, err = clientset.CoreV1().ConfigMaps(cfg.NameSpace).Update(configMap)
			if err != nil {
				log.Fatalf("Unable to update ConfigMap.")
			}

			log.Infof("Sleeping for %v", cfg.Sleep)
			time.Sleep(cfg.Sleep)
		}
	}
}
