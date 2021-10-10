
activate.touch: registry.touch docker/kubernetes.yml
	kubectl delete -f - < docker/kubernetes.yml|| true
	kubectl apply -f - < docker/kubernetes.yml

	touch activate.touch


registry.touch: image.touch
	podman push registry-snapshot.c.dockerutv12.tuv.jordbruksverket.se/kubernetes-pvcreator:0.1
	touch registry.touch

image.touch: kubernetes-pvcreator docker/Dockerfile
	cp kubernetes-pvcreator docker/
	podman build -t registry-snapshot.c.dockerutv12.tuv.jordbruksverket.se/kubernetes-pvcreator:0.1 docker
	touch image.touch

kubernetes-pvcreator: main.go
	go build .

.PHONY: test clean
test:
	kubectl apply -f - < docker/test/test.yml 
	kubectl wait --for=condition=ready --timeout=400s pod pvtester 
	kubectl exec -i pvtester ls /data
	kubectl delete -f - < docker/test/test.yml

cleantest:
	kubectl delete -f - < docker/test/test.yml > /dev/null || true
clean: cleantest
	rm -f registry.touch activate.touch image.touch kubernetes-pvcreator
