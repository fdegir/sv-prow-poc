#!/bin/bash
set -euo pipefail

declare -A hosts

{{- range .nodes }}
hosts[{{ .Hostname }}]={{ .Type }}
{{- end }}

HOSTNAME=$(cat /etc/hostname)
if [ ! "$HOSTNAME" ]; then
    HOSTNAME=$(cat /proc/sys/kernel/hostname)
    if [ ! "$HOSTNAME" ] || [ "$HOSTNAME" = "localhost.localdomain" ]; then
        echo "ERROR: Could not identify whether the host is a k3s server or agent due to missing hostname"
        exit 1
    fi
fi

NODETYPE="${hosts[$HOSTNAME]:-none}"
if [ "$NODETYPE" = "none" ]; then
    echo "ERROR: Could not identify whether host '$HOSTNAME' is a k3s server or agent"
    exit 1
fi

mount /var

mkdir -p /var/lib/rancher/k3s/agent/images/
cp {{ .imagesPath }}/* /var/lib/rancher/k3s/agent/images/

umount /var

CONFIGFILE={{ .configFilePath }}/$NODETYPE.yaml

if [ "$HOSTNAME" = {{ .initialiser }} ]; then
CONFIGFILE={{ .configFilePath }}/{{ .initialiserConfigFile }}

{{ if .manifestsPath }}
mkdir -p /opt/eib-k8s/manifests
cp {{ .manifestsPath }}/* /opt/eib-k8s/manifests/

cat <<- EOF > /etc/systemd/system/kubernetes-resources-install.service
[Unit]
Description=Kubernetes Resources Install
Requires=k3s.service
After=k3s.service
ConditionPathExists=/opt/bin/kubectl
ConditionPathExists=/etc/rancher/k3s/k3s.yaml

[Install]
WantedBy=multi-user.target

[Service]
Type=oneshot
Restart=on-failure
RestartSec=60
ExecStart=/opt/bin/kubectl apply -f /opt/eib-k8s/manifests --kubeconfig=/etc/rancher/k3s/k3s.yaml
# Disable the service and clean up
ExecStartPost=/bin/sh -c "systemctl disable kubernetes-resources-install.service"
ExecStartPost=rm -f /etc/systemd/system/kubernetes-resources-install.service
ExecStartPost=rm -rf /opt/eib-k8s
EOF

systemctl enable kubernetes-resources-install.service
{{- end }}
fi

{{- if and .apiVIP .apiHost }}
echo "{{ .apiVIP }} {{ .apiHost }}" >> /etc/hosts
{{- end }}

mkdir -p /etc/rancher/k3s/
cp $CONFIGFILE /etc/rancher/k3s/config.yaml

if [ -f {{ .registryMirrors }} ]; then
cp {{ .registryMirrors }} /etc/rancher/k3s/registries.yaml
fi

export INSTALL_K3S_EXEC=$NODETYPE
export INSTALL_K3S_SKIP_DOWNLOAD=true
export INSTALL_K3S_SKIP_START=true
export INSTALL_K3S_BIN_DIR=/opt/bin

mkdir -p $INSTALL_K3S_BIN_DIR
cp {{ .binaryPath }} $INSTALL_K3S_BIN_DIR/k3s
chmod +x $INSTALL_K3S_BIN_DIR/k3s

sh {{ .installScript }}
