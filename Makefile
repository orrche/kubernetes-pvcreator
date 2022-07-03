APPLICATION = reflinkpv

REGISTRY ?= mireg.wr25.org
REPOGROUP ?=
KUBECTLOPTS ?=
RELEASE ?= latest
DOCKERCOMMAND ?= podman


activate.touch: deployment.apply.yml
	kubectl $(KUBECTLOPTS) delete -f deployment.apply.yml || true
	kubectl $(KUBECTLOPTS) apply -f deployment.apply.yml

	touch activate.touch

deployment.apply.yml: docker.digest docker/kubernetes.yml
	cat docker/kubernetes.yml | sed "s#image: [^/]*/$(APPLICATION):.*#image: $(REGISTRY)$(REPOGROUP)/$(APPLICATION):$(RELEASE)@$$(cat docker.digest)#" > deployment.apply.yml


docker.digest: image.touch
	podman push $(REGISTRY)$(REPOGROUP)/$(APPLICATION):$(RELEASE)
	
	echo -n "sha256:" > docker.digest
	curl -s -H "Accept: application/vnd.docker.distribution.manifest.v2+json" https://$(REGISTRY)/v2$(REPOGROUP)/$(APPLICATION)/manifests/$(RELEASE) | sha256sum | awk '{print $$1}' >> docker.digest

image.touch: kubernetes-pvcreator docker/Dockerfile
	cp kubernetes-pvcreator docker/
	$(DOCKERCOMMAND) build -t $(REGISTRY)$(REPOGROUP)/$(APPLICATION):$(RELEASE) docker
	touch image.touch

kubernetes-pvcreator: main.go
	go test
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
	rm -f registry.touch activate.touch image.touch kubernetes-pvcreator docker.digest
