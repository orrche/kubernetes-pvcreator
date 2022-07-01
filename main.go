package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	SnapshotPath string `yaml:"SnapshotPath"`
	StorageClass string `yaml:"StorageClass"`
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

func getVolumeSnapshots(clientset *kubernetes.Clientset) {

	// vs, err := clientset
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

		cmd := exec.Command("rm", "-rf", pv.Spec.Local.Path)
		cmd.Start()
		cmd.Wait()
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
		if item.Spec.StorageClassName != nil && *item.Spec.StorageClassName != config.StorageClass {
			continue
		}
		if item.Status.Phase == "Pending" {
			fmt.Printf("  :: %s\n", item.ObjectMeta.Name)
			if item.Spec.DataSource == nil {
				continue
			}
			source := item.Spec.DataSource.Name
			fmt.Printf("  :: %s\n", source)
			created := false
			for _, pv := range pvs {
				if pv.Spec.ClaimRef == nil {
					fmt.Printf("Empty claim ref..")
					continue
				}
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
			path := filepath.Join(config.RootPath, guid.String())
			sourcePath := filepath.Join(config.SnapshotPath, source)
			fmt.Printf("Source: %s\n", sourcePath)

			cmd := exec.Command("test", "-d", sourcePath)
			_, err := cmd.CombinedOutput()
			if err != nil {
				continue
			}
			cmd = exec.Command("cp", "-rp", "--reflink=always", sourcePath, path)
			_, err = cmd.CombinedOutput()
			if err != nil {
				log.Println("Failed command: ", "cp", "-rp", "--reflink=always", sourcePath, path)
				continue
			}

			pv := v1.PersistentVolume{}
			volumeFile := v1.PersistentVolumeFilesystem
			pv.Spec.VolumeMode = &volumeFile
			pv.ObjectMeta = metav1.ObjectMeta{
				Name: guid.String(),
				Labels: map[string]string{
					"source": source,
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
						{Key: "kubernetes.io/hostname", Operator: "In", Values: []string{os.Getenv("NODE")}},
					},
				})
			_, err = clientset.CoreV1().PersistentVolumes().Create(context.TODO(), &pv, metav1.CreateOptions{})
			if err != nil {
				log.Print(err)
			}
		}
	}

}

func dumps() ([]Dump, error) {
	dump := []Dump{}

	files, err := ioutil.ReadDir(config.SnapshotPath)
	if err != nil {
		panic(err)
	}

	for _, folder := range files {
		d := Dump{}
		d.Name = folder.Name()
		d.Type = "unknown"

		jsonFile, err := os.Open(config.SnapshotPath + folder.Name() + ".meta")
		if err != nil {
			continue
		}
		byteValue, err := ioutil.ReadAll(jsonFile)
		if err != nil {
			continue
		}

		type dumpMeta struct {
			Type string `json:"type"`
		}

		dm := dumpMeta{}
		json.Unmarshal(byteValue, &dm)

		d.Type = dm.Type

		dump = append(dump, d)
	}

	// Sort by age, keeping original order or equal elements.
	sort.SliceStable(dump, func(i, j int) bool {
		return strings.Compare(dump[i].Name, dump[j].Name) == 1
	})

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
	for _, dumpSet := range state.dumps {
		if dumpSet.incomming.Add(30 * time.Second).After(time.Now()) {
			for _, dump := range dumpSet.Dumps {
				fmt.Fprintf(w, "%s,%s\n", dump.Name, dump.Type)
			}
		}
	}
}

type DumpSet struct {
	Dumps     []Dump `json:"dumps"`
	Source    string
	incomming time.Time
}
type State struct {
	dumps map[string]DumpSet
}

var state State

func Update(w http.ResponseWriter, r *http.Request) {

	res := DumpSet{}

	err := json.NewDecoder(r.Body).Decode(&res)
	if err != nil {
		panic(err)
	}

	for _, d := range res.Dumps {
		log.Printf("Dump %s reported from %s", d, res.Source)
		res.incomming = time.Now()
		uhm := state.dumps
		uhm[res.Source] = res
		state.dumps = uhm
		log.Printf("%+v", state)
	}
}

func CleanRunning(clientset *kubernetes.Clientset) {
	files, err := ioutil.ReadDir(config.RootPath)
	if err != nil {
		panic(err)
	}

	for _, folder := range files {
		fmt.Println(folder.Name())

		pvs := getPv(clientset)

		activeDump := false
		for _, pv := range pvs {
			if pv.Name == folder.Name() {
				activeDump = true
			}
		}
		if !activeDump {
			fmt.Printf("%s isnt active and should be deleted\n", folder.Name())
			// Maybe I should have some grace period thing here for newly created dumps..

			cmd := exec.Command("rm", "-rf", config.RootPath+folder.Name())
			cmd.Start()
			cmd.Wait()
		}
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
	if os.Getenv("REFLINKMASTER") == "true" {
		state.dumps = map[string]DumpSet{}

		http.HandleFunc("/getDumps.json", getDumpsJSON)
		http.HandleFunc("/getDumps.csv", getDumpsCSV)
		http.HandleFunc("/update", Update)
		http.ListenAndServe(":8080", nil)
	} else {
		for cont {
			CleanRunning(clientset)
			time.Sleep(10 * time.Second)
			process(clientset)

			d, err := dumps()
			if err != nil {
				continue
			}
			data := DumpSet{}
			data.Dumps = d
			data.Source = os.Getenv("NODE")

			json_data, err := json.Marshal(data)
			if err != nil {
				continue
			}
			client := http.Client{
				Timeout: 1 * time.Second,
			}
			client.Post("http://reflink:8080/update", "application/json", bytes.NewBuffer(json_data))
		}
	}
}
