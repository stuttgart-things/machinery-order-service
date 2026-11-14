import labels

config = labels.config

serviceAccount = {
    apiVersion: "v1"
    kind: "ServiceAccount"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
        if config.serviceAccountAnnotations:
            annotations: config.serviceAccountAnnotations
    }
}
