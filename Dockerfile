FROM centos:7

ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=utility

RUN mkdir -p /k8s-vgpu/bin
RUN mkdir -p /k8s-vgpu/lib

COPY _output/bin/linux/amd64/k8s-device-plugin /usr/bin/k8s-device-plugin
COPY _output/bin/linux/amd64/nvidia-container-runtime /k8s-vgpu/bin
COPY ./lib /k8s-vgpu/lib

ENTRYPOINT ["entrypoint.sh"]