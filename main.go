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
	ReportURL    string `yaml:"ReportURL"`
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
		if _, err := os.Stat(filepath.Join(config.RootPath, pv.Name)); !os.IsNotExist(err) {
			ret = append(ret, pv)
		}
	}

	return ret
}

func deletePv(clientset *kubernetes.Clientset, pv v1.PersistentVolume) {
	if strings.HasPrefix(pv.Spec.Local.Path, config.RootPath) {
		if _, err := os.Stat(pv.Spec.Local.Path); os.IsNotExist(err) {
			return
		}

		log.Printf("Deleting pv %s at %s", pv.ObjectMeta.Name, pv.Spec.Local.Path)
		clientset.CoreV1().PersistentVolumes().Delete(context.TODO(), pv.ObjectMeta.Name, metav1.DeleteOptions{})

		os.RemoveAll(pv.Spec.Local.Path)
	}
}

func process(clientset *kubernetes.Clientset, dClient dynamic.Interface) {
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
			if item.Spec.StorageClassName == nil || (*item.Spec.StorageClassName != config.StorageClass && *item.Spec.StorageClassName != "reflink0.5" && *item.Spec.StorageClassName != "manual") {
				log.Printf("Wrong storageclass for me '%s' needs '%s'", *item.Spec.StorageClassName, config.StorageClass)
				continue
			}
			var source string
			if item.Spec.DataSource == nil {
				log.Printf("No datasource in pvc: %s, this is odd", item.Name)
				log.Printf("%+v", item)

				if *item.Spec.StorageClassName != "manual" {
					continue
				}
			} else {
				source = item.Spec.DataSource.Name
			}
			labels := map[string]string{}

			var sourcePath string
			if *item.Spec.StorageClassName == "manual" || item.Spec.DataSource.Kind == "VolumeSnapshot" {
				if *item.Spec.StorageClassName == "reflink0.5" {
					sourcePath = filepath.Join(config.SnapshotPath, source)

					byteValue, err := ioutil.ReadFile(sourcePath + ".meta")
					if err != nil {
						log.Print(err)
						continue
					}

					var meta struct {
						Type string `json:"type"`
					}
					json.Unmarshal(byteValue, &meta)

					if meta.Type == "" {
						log.Printf("No meta type found for %s, aborting...", sourcePath)
						continue
					}

					labels["application"] = meta.Type

				} else if *item.Spec.StorageClassName == "manual" {
					if item.Spec.Selector == nil {
						// Really bad idea to use manual
						continue
					}
					source = item.Spec.Selector.MatchLabels["source"]
					sourcePath = filepath.Join(config.SnapshotPath, source)

					byteValue, err := ioutil.ReadFile(sourcePath + ".meta")
					if err != nil {
						log.Print(err)
						continue
					}

					var meta struct {
						Type string `json:"type"`
					}
					json.Unmarshal(byteValue, &meta)

					if meta.Type == "" {
						log.Printf("No meta type found for %s, aborting...", sourcePath)
						continue
					}

					labels["application"] = meta.Type

				} else {
					list := getLocalVolumeSnapshotContents(dClient)
					for _, item := range list {

						log.Print(item.Name)
						if item.Spec.VolumeSnapshotClassName == config.StorageClass && item.Spec.VolumeSnapshotRef.Name == source {
							sourcePath = item.Spec.Source.VolumeHandle
							labels = item.Labels
							break
						}
					}
				}
			} else {
				log.Printf("Unknown DataSource.Kind %s", item.Spec.DataSource.Kind)
				continue
			}
			if sourcePath == "" {
				log.Print("sourcePath not set, aborting")
				continue
			}
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
				Name:   guid.String(),
				Labels: labels,
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

var gdClient dynamic.Interface

func dumps() ([]Dump, error) {
	dump := []Dump{}
	if gdClient == nil {
		return dump, fmt.Errorf("not yet initialized")
	}

	for _, vc := range getLocalVolumeSnapshotContents(gdClient) {
		d := Dump{}
		d.Name = vc.Spec.VolumeSnapshotRef.Name
		d.Type = vc.Labels["application"]
		dump = append(dump, d)
	}

	files, err := ioutil.ReadDir(config.SnapshotPath)
	if err != nil {
		log.Fatal(err)
	}

	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".meta") {
			byteValue, err := ioutil.ReadFile(filepath.Join(config.SnapshotPath, f.Name()))
			if err != nil {
				log.Print(err)
				continue
			}

			var meta struct {
				Type string `json:"type"`
			}
			json.Unmarshal(byteValue, &meta)

			d := Dump{}
			d.Name = f.Name()[:len(f.Name())-5]
			d.Type = meta.Type

			dump = append(dump, d)
		}
	}

	return dump, nil
}

func getDumpsCSV(w http.ResponseWriter, r *http.Request) {
	dumps := []Dump{}

	for _, dump := range dumpCache.dumps {
		if dump.Creation.After(time.Now().Add(-2 * time.Minute)) {
			dumps = append(dumps, dump.Dumps...)
		}
	}

	for _, dump := range dumps {
		fmt.Fprintf(w, "%s,%s\n", dump.Name, dump.Type)
	}
}

var config Config

type VolumeSnapshotContent struct {
	Name       string
	Kind       string
	APIVersion string
	Labels     map[string]string

	Spec struct {
		VolumeSnapshotClassName string

		VolumeSnapshotRef struct {
			Name      string
			Namespace string
		}
		Source struct {
			VolumeHandle string
		}
	}
}

func getVolumeSnapshotContents(dClient dynamic.Interface) []VolumeSnapshotContent {
	ret := []VolumeSnapshotContent{}

	list := getUVolumeSnapshotContents(dClient)
	for _, uvcs := range list.Items {
		vcs := VolumeSnapshotContent{}

		vcs.Name = uvcs.GetName()
		vcs.Kind = uvcs.GetKind()
		vcs.Labels = uvcs.GetLabels()
		vcs.APIVersion = uvcs.GetAPIVersion()

		vcs.Spec.VolumeSnapshotClassName = uvcs.Object["spec"].(map[string]interface{})["volumeSnapshotClassName"].(string)
		if uvcs.Object["spec"].(map[string]interface{})["volumeSnapshotRef"] != nil {
			vcs.Spec.VolumeSnapshotRef.Name = uvcs.Object["spec"].(map[string]interface{})["volumeSnapshotRef"].(map[string]interface{})["name"].(string)
			vcs.Spec.VolumeSnapshotRef.Namespace = uvcs.Object["spec"].(map[string]interface{})["volumeSnapshotRef"].(map[string]interface{})["namespace"].(string)
		}
		if uvcs.Object["spec"].(map[string]interface{})["source"] != nil {
			vcs.Spec.Source.VolumeHandle = uvcs.Object["spec"].(map[string]interface{})["source"].(map[string]interface{})["volumeHandle"].(string)
		}

		ret = append(ret, vcs)

	}

	return ret
}
func getUVolumeSnapshotContents(dClient dynamic.Interface) *unstructured.UnstructuredList {
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

func getUVolumeSnapshots(dClient dynamic.Interface, namespace string) *unstructured.UnstructuredList {
	volumesnapshotRes := schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}

	// List VolumeSnapshots
	list, err := dClient.Resource(volumesnapshotRes).Namespace(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err)
	}

	return list
}

type VolumeSnapshot struct {
	name            string
	namespace       string
	labels          map[string]string
	resourceVersion string
	spec            struct {
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

func processVolumeSnapshot(clientset *kubernetes.Clientset, dClient dynamic.Interface, vs VolumeSnapshot) {
	volumesnapshotcontentRes := schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotcontents"}
	volumesnapshotRes := schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}

	// Check if already created
	if vs.status.boundVolumeSnapshotContentName != "" {
		return
	}

	lpvs := getLocalPVs(clientset)
	for _, lpv := range lpvs {
		if lpv.Spec.ClaimRef.Name == vs.spec.source.persistentVolumeClaimName {
			name := vs.name + uuid.New().String()
			destination := filepath.Join(config.SnapshotPath, name)

			cmd := exec.Command("cp", "-rp", "--reflink=always", lpv.Spec.Local.Path, destination)
			cmd.Start()
			cmd.Wait()
			log.Print(cmd.String())

			labels := lpv.GetLabels()
			for k, v := range vs.labels {
				labels[k] = v
			}

			VSContent := &unstructured.Unstructured{}
			VSContent.SetUnstructuredContent(map[string]interface{}{
				"apiVersion": "snapshot.storage.k8s.io/v1",
				"kind":       "VolumeSnapshotContent",
				"metadata": map[string]interface{}{
					"name":   name,
					"labels": labels,
				},
				"spec": map[string]interface{}{
					"deletionPolicy": "Delete",
					"driver":         config.StorageClass,
					"source": map[string]interface{}{
						"volumeHandle": destination,
					},
					"sourceVolumeMode":        "Filesystem",
					"volumeSnapshotClassName": config.StorageClass,
					"volumeSnapshotRef": map[string]interface{}{
						"name":      vs.name,
						"namespace": vs.namespace,
					},
				},
			})
			_, err := dClient.Resource(volumesnapshotcontentRes).Create(context.TODO(), VSContent, metav1.CreateOptions{})
			if err != nil {
				panic(err)
			}

			mdb := &unstructured.Unstructured{}
			mdb.SetUnstructuredContent(map[string]interface{}{
				"apiVersion": "snapshot.storage.k8s.io/v1",
				"kind":       "VolumeSnapshot",
				"metadata": map[string]interface{}{
					"name":            vs.name,
					"namespace":       vs.namespace,
					"resourceVersion": vs.resourceVersion,
				},
				"status": map[string]interface{}{
					"readyToUse":                     true,
					"boundVolumeSnapshotContentName": name,
					"creationTime":                   time.Now().Format(time.RFC3339),
				},
			})

			_, err = dClient.Resource(volumesnapshotRes).Namespace(vs.namespace).UpdateStatus(context.TODO(), mdb, metav1.UpdateOptions{})
			if err != nil {
				panic(err)
			}
		}
	}
}

func processVolumeSnapshots(clientset *kubernetes.Clientset, dClient dynamic.Interface) {
	namespaces, err := clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err)
	}
	for _, namespace := range namespaces.Items {
		list := getUVolumeSnapshots(dClient, namespace.GetName())

		for _, d := range list.Items {
			vs := VolumeSnapshot{}
			vs.labels = d.GetLabels()
			vs.name = d.GetName()
			vs.namespace = d.GetNamespace()
			vs.spec.volumeSnapshotClassName = d.Object["spec"].(map[string]interface{})["volumeSnapshotClassName"].(string)
			vs.spec.source.persistentVolumeClaimName = d.Object["spec"].(map[string]interface{})["source"].(map[string]interface{})["persistentVolumeClaimName"].(string)
			vs.spec.source.persistentVolumeClaimName = d.Object["spec"].(map[string]interface{})["source"].(map[string]interface{})["persistentVolumeClaimName"].(string)
			vs.resourceVersion = d.GetResourceVersion()
			if d.Object["status"] != nil {
				vs.status.boundVolumeSnapshotContentName = d.Object["status"].(map[string]interface{})["boundVolumeSnapshotContentName"].(string)
				vs.status.readyToUse = d.Object["status"].(map[string]interface{})["readyToUse"].(bool)
				vs.status.creationTime = d.Object["status"].(map[string]interface{})["creationTime"].(string)
			}

			processVolumeSnapshot(clientset, dClient, vs)
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
			log.Printf("Deleting '%s'", filepath.Join(config.RootPath, f.Name()))
			os.RemoveAll(filepath.Join(config.RootPath, f.Name()))
		}
	}

}

func CleanSnapshots(clientset *kubernetes.Clientset, dClient dynamic.Interface) {
	files, err := ioutil.ReadDir(config.SnapshotPath)
	if err != nil {
		log.Fatal(err)
	}

	vscs := getLocalVolumeSnapshotContents(dClient)
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
		for _, vsc := range vscs {
			if vsc.Spec.Source.VolumeHandle == filepath.Join(config.SnapshotPath, f.Name()) {
				found = true
			}
		}

		if !found {
			log.Printf("Deleting '%s'", f.Name())
			os.RemoveAll(filepath.Join(config.SnapshotPath, f.Name()))
		}

	}

}

func getLocalVolumeSnapshotContents(dClient dynamic.Interface) []VolumeSnapshotContent {
	vcs := getVolumeSnapshotContents(dClient)
	ret := []VolumeSnapshotContent{}

	for _, vc := range vcs {
		if vc.Spec.VolumeSnapshotClassName != config.StorageClass {
			continue
		}
		if _, err := os.Stat(vc.Spec.Source.VolumeHandle); os.IsNotExist(err) {
			continue
		}

		ret = append(ret, vc)
	}
	return ret
}

type Report struct {
	Dumps    []Dump `json:"dump"`
	Provider string `json:"prodiver"`
	Creation time.Time
}

func RepportVolumeContent(dClient dynamic.Interface) {
	rep := Report{}
	var err error
	rep.Provider, err = os.Hostname()
	if err != nil {
		return
	}
	rep.Dumps, err = dumps()
	if err != nil {
		return
	}

	json_data, err := json.Marshal(rep)

	if err != nil {
		log.Fatal(err)
	}

	_, err = http.Post(config.ReportURL+"/update", "application/json",
		bytes.NewBuffer(json_data))

	if err != nil {
		log.Fatal(err)
	}
}

func updateDumps(w http.ResponseWriter, r *http.Request) {
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Panic(err)
	}

	rep := Report{}

	json.Unmarshal(data, &rep)
	rep.Creation = time.Now()
	dumpCache.dumps[rep.Provider] = rep

	log.Printf("%+v", dumpCache)
}

var dumpCache struct {
	dumps map[string]Report
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
	gdClient = dClient
	yamlFile, err := ioutil.ReadFile("conf.yml")
	if err != nil {
		log.Panic(err)
	}
	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		log.Panic(err)
	}
	if _, err := os.Stat(config.RootPath); os.IsNotExist(err) {
		log.Printf("Creating directory %s", config.RootPath)
		err := os.MkdirAll(config.RootPath, os.ModePerm)
		if err != nil {
			panic(err)
		}
	}
	if _, err := os.Stat(config.SnapshotPath); os.IsNotExist(err) {
		log.Printf("Creating directory %s", config.SnapshotPath)
		err := os.MkdirAll(config.SnapshotPath, os.ModePerm)
		if err != nil {
			panic(err)
		}
	}

	if os.Getenv("REFLINKMASTER") == "true" {
		log.Print("MASTER MODE")
		dumpCache.dumps = map[string]Report{}
		http.HandleFunc("/update", updateDumps)
		http.HandleFunc("/getDumps.csv", getDumpsCSV)
		http.ListenAndServe(":8080", nil)
	} else {
		for {
			CleanDumps(clientset)
			CleanSnapshots(clientset, dClient)
			process(clientset, dClient)
			processVolumeSnapshots(clientset, dClient)
			RepportVolumeContent(dClient)
			time.Sleep(10 * time.Second)
		}
	}
}
