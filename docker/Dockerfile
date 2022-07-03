FROM fedora:32

RUN dnf install -y openssh-clients

RUN adduser -m pvcreator

WORKDIR /home/pvcreator/conf.yml
ADD kubernetes-pvcreator /kubernetes-pvcreator

# USER pvcreator

CMD ["/kubernetes-pvcreator"]
