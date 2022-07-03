module kubernetes-pvcreator

go 1.14

replace k8s.io/client-go => k8s.io/client-go v0.20.0

require (
	github.com/google/uuid v1.3.0
	gopkg.in/yaml.v2 v2.4.0
	k8s.io/api v0.20.0
	k8s.io/apimachinery v0.20.0
	k8s.io/client-go v0.20.0
)
