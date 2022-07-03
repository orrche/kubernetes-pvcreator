package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type Config struct {
	RootPath     string `yaml:"RootPath"`
	StorageClass string `yaml:"StorageClass"`
	SnapshotPath string `yaml:"SnapshotPath"`
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

func getLocalPVs(clientset *kubernetes.Clientset) []v1.PersistentVolume {
	ret := []v1.PersistentVolume{}
	pvs := getPv(clientset)

	for _, pv := range pvs {
		if _, err := os.Stat(config.RootPath + pv.Name); !os.IsNotExist(err) {
			ret = append(ret, pv)
		}
	}

	return ret
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
		if _, err := os.Stat(pv.Spec.Local.Path); os.IsNotExist(err) {
			return
		}

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
		if item.Status.Phase == "Pending" {
			fmt.Printf("  :: %s\n", item.ObjectMeta.Name)
			if item.Spec.StorageClassName == nil || *item.Spec.StorageClassName != config.StorageClass {
				log.Printf("Wrong storageclass for me '%s' needs '%s'", *item.Spec.StorageClassName, config.StorageClass)
				continue
			}
			if item.Spec.DataSource == nil {
				log.Printf("No datasource, this is odd")
				continue
			}

			source := item.Spec.DataSource.Name
			sourcePath := filepath.Join(config.SnapshotPath, source)
			fmt.Printf("Aiming to snapshot :: %s\n", sourcePath)
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

			if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
				// path/to/whatever does not exist
				log.Printf("I dont have %s", sourcePath)
				continue
			}

			cmd := exec.Command("cp", "-rp", "--reflink=always", sourcePath, path)
			_, err := cmd.CombinedOutput()
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

type VolumeSnapshotContent struct {
}

func getUVolumeSnapshotContent(dClient dynamic.Interface) *unstructured.UnstructuredList {
	volumesnapshotcontentRes := schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotcontents"}

	// VSContent := &unstructured.Unstructured{}
	//	VSContent.SetUnstructuredContent(map[string]interface{}{
	//		"apiVersion": "snapshot.storage.k8s.io/v1",
	//		"kind":       "VolumeSnapshotContent",
	//		"metadata": map[string]interface{}{
	//			"name": name,
	//			"labels": map[string]interface{}{
	//				"node": os.Getenv("NODE"),
	//			},
	//		},
	//		"spec": map[string]interface{}{
	//			"deletionPolicy": "Delete",
	//			"driver":         config.StorageClass,
	//			"source": map[string]interface{}{
	//				"volumeHandle": lpv.Name,
	//			},
	//			"sourceVolumeMode":        "Filesystem",
	//			"volumeSnapshotClassName": config.StorageClass,
	//			"volumeSnapshotRef": map[string]interface{}{
	//				"name":      vs.name,
	//				"namespace": d.GetNamespace(),
	//			},
	//		},
	//	})
	list, err := dClient.Resource(volumesnapshotcontentRes).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err)
	}

	return list
}

func getVolumeSnapshots(clientset *kubernetes.Clientset, dClient dynamic.Interface) {

	namespace := "sandbox-kgusw"

	volumesnapshotRes := schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
	volumesnapshotcontentRes := schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotcontents"}

	// List VolumeSnapshots
	list, err := dClient.Resource(volumesnapshotRes).Namespace(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err)
	}

	for _, d := range list.Items {
		type VolumeSnapshot struct {
			name string
			spec struct {
				volumeSnapshotClassName string
				source                  struct {
					persistentVolumeClaimName string
				}
			}
			status struct {
				readyToUse                     bool
				boundVolumeSnapshotContentName string
				creationTime                   string
			}
		}

		vs := VolumeSnapshot{}
		vs.name = d.GetName()
		vs.spec.volumeSnapshotClassName = d.Object["spec"].(map[string]interface{})["volumeSnapshotClassName"].(string)
		vs.spec.source.persistentVolumeClaimName = d.Object["spec"].(map[string]interface{})["source"].(map[string]interface{})["persistentVolumeClaimName"].(string)
		if d.Object["status"] != nil {
			vs.status.boundVolumeSnapshotContentName = d.Object["status"].(map[string]interface{})["boundVolumeSnapshotContentName"].(string)
			vs.status.readyToUse = d.Object["status"].(map[string]interface{})["readyToUse"].(bool)
			vs.status.creationTime = d.Object["status"].(map[string]interface{})["creationTime"].(string)
		}

		log.Printf("%+v", vs)

		// Check if already created
		if vs.status.boundVolumeSnapshotContentName != "" {
			continue
		}

		lpvs := getLocalPVs(clientset)
		for _, lpv := range lpvs {
			if lpv.Spec.ClaimRef.Name == vs.spec.source.persistentVolumeClaimName {
				log.Print("Yay we have that one, we should do stuff")
				name := vs.name + uuid.New().String()
				destination := filepath.Join(config.SnapshotPath, name)

				cmd := exec.Command("cp", "-rp", "--reflink=always", lpv.Spec.Local.Path, destination)
				cmd.Start()
				cmd.Wait()
				log.Print(cmd.String())

				VSContent := &unstructured.Unstructured{}
				VSContent.SetUnstructuredContent(map[string]interface{}{
					"apiVersion": "snapshot.storage.k8s.io/v1",
					"kind":       "VolumeSnapshotContent",
					"metadata": map[string]interface{}{
						"name": name,
						"labels": map[string]interface{}{
							"node": os.Getenv("NODE"),
						},
					},
					"spec": map[string]interface{}{
						"deletionPolicy": "Delete",
						"driver":         config.StorageClass,
						"source": map[string]interface{}{
							"volumeHandle": lpv.Name,
						},
						"sourceVolumeMode":        "Filesystem",
						"volumeSnapshotClassName": config.StorageClass,
						"volumeSnapshotRef": map[string]interface{}{
							"name":      vs.name,
							"namespace": d.GetNamespace(),
						},
					},
				})
				_, err = dClient.Resource(volumesnapshotcontentRes).Create(context.TODO(), VSContent, metav1.CreateOptions{})
				if err != nil {
					panic(err)
				}

				mdb := &unstructured.Unstructured{}
				mdb.SetUnstructuredContent(map[string]interface{}{
					"apiVersion": "snapshot.storage.k8s.io/v1",
					"kind":       "VolumeSnapshot",
					"metadata": map[string]interface{}{
						"name":            d.GetName(),
						"namespace":       d.GetNamespace(),
						"resourceVersion": d.GetResourceVersion(),
					},
					"status": map[string]interface{}{
						"readyToUse":                     true,
						"boundVolumeSnapshotContentName": name,
						"creationTime":                   time.Now().Format(time.RFC3339),
					},
				})

				_, err := dClient.Resource(volumesnapshotRes).Namespace(d.GetNamespace()).UpdateStatus(context.TODO(), mdb, metav1.UpdateOptions{})
				if err != nil {
					panic(err)
				}
			}
		}

	}
}

func CleanDumps(clientset *kubernetes.Clientset) {
	files, err := ioutil.ReadDir(config.RootPath)
	if err != nil {
		log.Fatal(err)
	}

	localPvs := getLocalPVs(clientset)
	for _, f := range files {
		found := false
		for _, pvs := range localPvs {
			if pvs.Name == f.Name() {
				found = true
				break
			}
		}
		if !found {
			cmd := exec.Command("rm", "-rf", filepath.Join(config.RootPath, f.Name()))
			cmd.Start()
			cmd.Wait()

		}
	}

}

func CleanSnapshots(clientset *kubernetes.Clientset, dClient dynamic.Interface) {
	files, err := ioutil.ReadDir(config.SnapshotPath)
	if err != nil {
		log.Fatal(err)
	}

	volumeSnapshtoContent := getUVolumeSnapshotContent(dClient)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".meta") {
			// Old style meta file, ignore for now
			continue
		}
		if _, err := os.Stat(filepath.Join(config.SnapshotPath, f.Name()+".meta")); !os.IsNotExist(err) {
			// Old style folder with meta, ignore for now
			continue
		}

		found := false
		for _, vsc := range volumeSnapshtoContent.Items {
			if vsc.GetName() == f.Name() {
				found = true
			}
		}

		if !found {
			log.Printf("Deleting '%s'", f.Name())
			cmd := exec.Command("rm", "-rf", filepath.Join(config.SnapshotPath, f.Name()))
			cmd.Start()
			cmd.Wait()
		}

	}

}

func main() {
	kConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Panic(err)
	}

	clientset, err := kubernetes.NewForConfig(kConfig)
	if err != nil {
		log.Panic(err)
	}
	dClient, err := dynamic.NewForConfig(kConfig)
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
		CleanDumps(clientset)
		CleanSnapshots(clientset, dClient)
		process(clientset)
		getVolumeSnapshots(clientset, dClient)
	}
}
