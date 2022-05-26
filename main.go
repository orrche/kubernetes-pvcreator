package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Config struct {
	RootPath     string `yaml:"RootPath"`
	StorageClass string `yaml:"StorageClass"`
	Nodes        []struct {
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

type Dump struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func getPvc(clientset *kubernetes.Clientset) []v1.PersistentVolumeClaim {
	pvc, err := clientset.CoreV1().PersistentVolumeClaims("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Print(err)
	}
	return pvc.Items
}

func getPv(clientset *kubernetes.Clientset) []v1.PersistentVolume {
	pv, err := clientset.CoreV1().PersistentVolumes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Print(err)
	}
	return pv.Items
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
func deletePv(clientset *kubernetes.Clientset, pv v1.PersistentVolume) {

	if startsWith(pv.Spec.Local.Path, config.RootPath) {
		fmt.Printf(":: %s[%s] - %s\n", pv.ObjectMeta.Name, pv.ObjectMeta.Labels["source"], pv.Status.Phase)
		clientset.CoreV1().PersistentVolumes().Delete(context.TODO(), pv.ObjectMeta.Name, metav1.DeleteOptions{})

		for _, server := range config.Nodes {
			cmd := exec.Command("ssh", server.Host, "sudo", "rm", "-rf", pv.Spec.Local.Path)
			cmd.Start()
			cmd.Wait()
		}
	}
}

func process(clientset *kubernetes.Clientset) {
	pvs := getPv(clientset)

	for _, pv := range pvs {
		if pv.Status.Phase == "Failed" {
			deletePv(clientset, pv)
		}
	}

	items := getPvc(clientset)
	for _, item := range items {
		if item.Status.Phase == "Pending" {
			fmt.Printf("  :: %s\n", item.ObjectMeta.Name)
			if item.Spec.Selector == nil {
				continue
			}
			source, ok := item.Spec.Selector.MatchLabels["source"]
			if !ok {
				continue
			}
			fmt.Printf("  :: %s\n", source)
			created := false
			for _, pv := range pvs {
				if item.ObjectMeta.Namespace == pv.Spec.ClaimRef.Namespace &&
					item.ObjectMeta.Name == pv.Spec.ClaimRef.Name {
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
				cmd := exec.Command("ssh", node.Host, "test", "-d", config.RootPath+"/dump/"+item.Spec.Selector.MatchLabels["source"])
				_, err := cmd.CombinedOutput()
				if err != nil {
					continue
				}
				cmd = exec.Command("ssh", node.Host, "sudo", "cp", "-rp", "--reflink=always", config.RootPath+"/dump/"+item.Spec.Selector.MatchLabels["source"], path)
				_, err = cmd.CombinedOutput()
				if err != nil {
					log.Println("Failed command: ", "ssh", node.Host, "sudo", "cp", "-rp", "--reflink=always", config.RootPath+"/dump/"+item.Spec.Selector.MatchLabels["source"], path)
					continue
				}
				hosts = append(hosts, node.Hostname)
			}
			if len(hosts) == 0 {
				continue
			}

			pv := v1.PersistentVolume{}
			volumeFile := v1.PersistentVolumeFilesystem
			pv.Spec.VolumeMode = &volumeFile
			pv.ObjectMeta = metav1.ObjectMeta{
				Name: guid.String(),
				Labels: map[string]string{
					"source": item.Spec.Selector.MatchLabels["source"],
				},
			}
			pv.Spec.Capacity = v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): *resource.NewQuantity(int64(3000000000000), resource.BinarySI),
			}

			pv.Spec.AccessModes = []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}
			pv.Spec.PersistentVolumeReclaimPolicy = v1.PersistentVolumeReclaimDelete
			pv.Spec.StorageClassName = config.StorageClass
			pv.Spec.ClaimRef = &v1.ObjectReference{
				Name:      item.ObjectMeta.Name,
				Namespace: item.ObjectMeta.Namespace,
			}
			pv.Spec.Local = &v1.LocalVolumeSource{
				Path: path,
			}
			pv.Spec.NodeAffinity = &v1.VolumeNodeAffinity{
				Required: &v1.NodeSelector{},
			}

			pv.Spec.NodeAffinity.Required.NodeSelectorTerms = append(pv.Spec.NodeAffinity.Required.NodeSelectorTerms,
				v1.NodeSelectorTerm{
					MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: "kubernetes.io/hostname", Operator: "In", Values: hosts},
					},
				})
			_, err := clientset.CoreV1().PersistentVolumes().Create(context.TODO(), &pv, metav1.CreateOptions{})
			if err != nil {
				log.Print(err)
			}
		}
	}

}

func dumps() ([]Dump, error) {
	dump := []Dump{}

	for _, node := range config.Nodes {
		cmd := exec.Command("ssh", node.Host, "ls", config.RootPath+"/dump/")
		output, err := cmd.CombinedOutput()
		if err != nil {
			continue
		}

		for _, folder := range strings.Split(string(output), "\n") {
			log.Print(folder)
			d := Dump{}
			d.Name = folder
			d.Type = "unknown"

			cmd := exec.Command("ssh", node.Host, "cat", config.RootPath+"/dump/"+folder+".meta")
			metaInfo, err := cmd.CombinedOutput()
			if err != nil {
				continue
			}

			type dumpMeta struct {
				Type string `json:"type"`
			}

			dm := dumpMeta{}
			json.Unmarshal(metaInfo, &dm)

			d.Type = dm.Type

			dump = append(dump, d)
		}
	}
	return dump, nil
}

func getDumpsJSON(w http.ResponseWriter, r *http.Request) {

	type Result struct {
		Dumps []Dump `json:"dumps"`
	}

	res := Result{}
	var err error
	res.Dumps, err = dumps()
	if err != nil {
		panic(err)
	}

	b, err := json.Marshal(res)
	if err != nil {
		panic(err)
	}

	w.Write(b)
}

func getDumpsCSV(w http.ResponseWriter, r *http.Request) {
	dumps, err := dumps()
	if err != nil {
		panic(err)
	}
	for _, dump := range dumps {
		fmt.Fprintf(w, "%s,%s\n", dump.Name, dump.Type)
	}
}

var config Config

func main() {
	kConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Panic(err)
	}

	clientset, err := kubernetes.NewForConfig(kConfig)
	if err != nil {
		log.Panic(err)
	}

	yamlFile, err := ioutil.ReadFile("conf.yml")
	if err != nil {
		log.Panic(err)
	}
	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		log.Panic(err)
	}

	cont := true
	go func() {
		http.HandleFunc("/getDumps.json", getDumpsJSON)
		http.HandleFunc("/getDumps.csv", getDumpsCSV)
		http.ListenAndServe(":8080", nil)
		cont = false
	}()

	for cont {
		time.Sleep(10 * time.Second)
		process(clientset)
	}
}
