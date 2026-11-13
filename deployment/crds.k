"""
CRDs and the ClusterRole, read from deployment/generated (`task generate`
copies them from config/crd and config/rbac).
"""

import file
import yaml
import labels

config = labels.config

_dir = file.modpath() + "/generated"

crds = [
    yaml.decode(file.read(_dir + "/order.machinery.stuttgart-things.com_orders.yaml"))
    yaml.decode(file.read(_dir + "/order.machinery.stuttgart-things.com_managementclusters.yaml"))
] if config.installCRDs else []

_role = yaml.decode(file.read(_dir + "/role.yaml"))

clusterRole = {
    apiVersion: "rbac.authorization.k8s.io/v1"
    kind: "ClusterRole"
    metadata: {
        name: config.name
        labels: labels.commonLabels
    }
    rules: _role.rules
}

clusterRoleBinding = {
    apiVersion: "rbac.authorization.k8s.io/v1"
    kind: "ClusterRoleBinding"
    metadata: {
        name: config.name
        labels: labels.commonLabels
    }
    roleRef: {
        apiGroup: "rbac.authorization.k8s.io"
        kind: "ClusterRole"
        name: config.name
    }
    subjects: [{
        kind: "ServiceAccount"
        name: config.name
        namespace: config.namespace
    }]
}
