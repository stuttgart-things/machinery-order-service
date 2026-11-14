import base64
import labels

config = labels.config

secret = {
    apiVersion: "v1"
    kind: "Secret"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
    }
    type: "Opaque"
    data: {k: base64.encode(v) for k, v in config.secrets}
} if config.secrets else None
