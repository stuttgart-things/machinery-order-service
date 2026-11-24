"""
Manifests of machinery-order-service
"""

import manifests as k8s
import namespace
import crds
import serviceaccount
import configmap
import secret
import deploy
import service
import ingress
import httproute
import grpcroute
import managementclusters

_manifests = [m for m in [
    namespace.namespace
] + crds.crds + [
    serviceaccount.serviceAccount
    crds.clusterRole
    crds.clusterRoleBinding
    configmap.configMap
    secret.secret
    deploy.deployment
    service.service
    ingress.ingress
    httproute.httproute
    grpcroute.grpcroute
] + managementclusters.managementClusters if m]

k8s.yaml_stream(_manifests)
