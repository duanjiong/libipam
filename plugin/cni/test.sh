export CNI_PATH=/opt/cni/bin/
export NETCONFPATH=/etc/cni/net.d/
export CNI_IFNAME=eth0
export CNI_ARGS="IgnoreUnknown=true;Type=macvtap;K8S_POD_NAMESPACE=hha;K8S_POD_INFRA_VM_ID=bibi;Pool=testippool"
#cnitool $1 ipam xxxxxxx
#cnitool add ipam xxxxxxx
#cnitool del ipam xxxxxxx