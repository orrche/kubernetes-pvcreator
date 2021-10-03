package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v2"
)

type Config struct {
	RootPath string `yaml:"RootPath"`
	Nodes    []struct {
		Hostname string `yaml:"Hostname"` // Hostname whatever the node is named in kubernetes
		Host     string `yaml:"Host"`     // Host whatever the node is reached at with ssh
	} `yaml:"Nodes"`
}

type Item struct {
	Kind     string `json:"kind"`
	MetaData struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`

	Spec struct {
		Selector struct {
			MatchLabels struct {
				Source string `json:"source"`
			} `json:"matchLabels"`
		} `json:"selector"`
	} `json:"spec"`

	Status struct {
		Phase string `json:"phase"`
	}
}

type PersistentVolume struct {
	Spec struct {
		Local struct {
			Path string `json:"path"`
		} `json:"local"`
		ClaimRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			Kind      string `json:"kind"`
		}
	} `json:"spec"`

	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`

	MetaData struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

func getPvc() []Item {
	cmd := exec.Command("kubectl", "get", "pvc", "-o", "json")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	var response struct {
		APIVersion string `json:"apiVersion"`

		Items []Item `json:"items"`
	}
	if err := json.NewDecoder(stdout).Decode(&response); err != nil {
		log.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		log.Fatal(err)
	}
	return response.Items
}

func getPv() []PersistentVolume {
	cmd := exec.Command("ssh", "root@192.168.0.214", "kubectl", "get", "pv", "-o", "json")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	var response struct {
		APIVersion string `json:"apiVersion"`

		Items []PersistentVolume `json:"items"`
	}
	if err := json.NewDecoder(stdout).Decode(&response); err != nil {
		log.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		log.Fatal(err)
	}

	return response.Items
}

func startsWith(a, b string) bool {
	if len(a) < len(b) {
		return false
	}
	if a[:len(b)] == b {
		return true
	}
	return false
}
func deletePv(pv PersistentVolume) {

	if startsWith(pv.Spec.Local.Path, config.RootPath) {
		fmt.Printf(":: %s[%s] - %s\n", pv.MetaData.Name, pv.MetaData.Labels["source"], pv.Status.Phase)
		cmd := exec.Command("ssh", "root@192.168.0.214", "kubectl", "delete", "pv", pv.MetaData.Name)
		cmd.Start()
		cmd.Wait()

		for _, server := range config.Nodes {
			cmd = exec.Command("ssh", "root@"+server.Host, "rm", "-rf", pv.Spec.Local.Path)
			cmd.Start()
			cmd.Wait()
		}
	}
}

func process() {
	pvs := getPv()

	for _, pv := range pvs {
		if pv.Status.Phase == "Failed" {
			deletePv(pv)
		}
	}

	pvTemplate := `
apiVersion: v1
kind: PersistentVolume
metadata:
  name: {{ .GUID }}
  labels:
    source: {{ .Selector }}
spec:
  capacity:
    storage: 100Gi
  volumeMode: Filesystem
  accessModes:
  - ReadWriteOnce
  persistentVolumeReclaimPolicy: Delete
  storageClassName: manual
  claimRef:
    name: {{ .PVC }}
    namespace: {{ .Namespace }}
  local:
    path: {{ .Path }}
  nodeAffinity:
          required:
                  nodeSelectorTerms:
                          - matchExpressions:
                                  - key: kubernetes.io/hostname
                                    operator: In
                                    values:
                                            - {{ .Hostname }}
`

	templ := template.Must(template.New("pv").Parse(pvTemplate))
	items := getPvc()
	for _, item := range items {
		if item.Status.Phase == "Pending" {
			fmt.Printf("  :: %s\n", item.MetaData.Name)
			fmt.Printf("  :: %s\n", item.Spec.Selector.MatchLabels.Source)
			created := false
			for _, pv := range pvs {
				if item.MetaData.Namespace == pv.Spec.ClaimRef.Namespace &&
					item.MetaData.Name == pv.Spec.ClaimRef.Name {
					fmt.Println("This one is already created")
					created = true
				}
			}
			if created {
				continue
			}
			guid := uuid.New()
			path := fmt.Sprintf("%s/%s", config.RootPath, guid)

			hosts := []string{}
			for _, node := range config.Nodes {
				cmd := exec.Command("ssh", "root@"+node.Host, "test", "-d", config.RootPath+"/dump/"+item.Spec.Selector.MatchLabels.Source)
				_, err := cmd.CombinedOutput()
				if err != nil {
					continue
				}
				cmd = exec.Command("ssh", "root@"+node.Host, "cp", "-rp", "--reflink=always", config.RootPath+"/dump/"+item.Spec.Selector.MatchLabels.Source, path)
				_, err = cmd.CombinedOutput()
				if err != nil {
					log.Panic(err)
				}
				hosts = append(hosts, node.Hostname)
			}
			if len(hosts) == 0 {
				continue
			}
			cmd := exec.Command("ssh", "root@192.168.0.214", "kubectl", "apply", "-f", "-")
			stdin, err := cmd.StdinPipe()

			go func() {
				defer stdin.Close()
				var data struct {
					Path      string
					Selector  string
					Hostname  string
					GUID      string
					Namespace string
					PVC       string
				}

				data.Path = path
				data.Selector = item.Spec.Selector.MatchLabels.Source
				data.Hostname = "localhost.localdomain"
				data.GUID = guid.String()
				data.Namespace = item.MetaData.Namespace
				data.PVC = item.MetaData.Name
				templ.Execute(os.Stdout, data)
				err := templ.Execute(stdin, data)
				if err != nil {
					log.Panic(err)
				}
			}()

			out, err := cmd.CombinedOutput()
			if err != nil {
				log.Panic(err)
			}

			fmt.Printf("%s\n", out)
		}
	}

}

var config Config

func main() {
	yamlFile, err := ioutil.ReadFile("conf.yaml")
	if err != nil {
		log.Panic(err)
	}
	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		log.Panic(err)
	}

	for {
		time.Sleep(10 * time.Second)
		process()
	}
}
