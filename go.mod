module kubernetes-pvcreator

go 1.14

replace k8s.io/client-go => k8s.io/client-go v0.20.0

require (
	github.com/dgrijalva/jwt-go v3.2.0+incompatible // indirect
	github.com/google/uuid v1.3.0
	github.com/gregjones/httpcache v0.0.0-20180305231024-9cad4c3443a7 // indirect
	github.com/imdario/mergo v0.3.5 // indirect
	github.com/pborman/uuid v1.2.1 // indirect
	github.com/peterbourgon/diskv v2.0.1+incompatible // indirect
	github.com/ugorji/go v1.2.6 // indirect
	gopkg.in/yaml.v2 v2.4.0
	k8s.io/api v0.20.0
	k8s.io/apimachinery v0.20.0
	k8s.io/client-go v0.20.0
)
