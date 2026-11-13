"""
ManagementCluster objects from config.managementClusters (name -> spec)
"""

import labels

config = labels.config

managementClusters = [{
    apiVersion: "order.machinery.stuttgart-things.com/v1alpha1"
    kind: "ManagementCluster"
    metadata: {
        name: n
        labels: labels.commonLabels
    }
    spec: s
} for n, s in config.managementClusters]
