import labels

config = labels.config

ingress = {
    apiVersion: "networking.k8s.io/v1"
    kind: "Ingress"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
        if config.ingressAnnotations:
            annotations: config.ingressAnnotations
    }
    spec: {
        ingressClassName: config.ingressClassName
        rules: [{
            host: config.ingressHost
            http.paths: [{
                path: "/"
                pathType: "Prefix"
                backend.service: {name: config.name, port.number: config.servicePort}
            }]
        }]
        if config.ingressTlsEnabled:
            tls: [{hosts: [config.ingressHost], secretName: config.ingressTlsSecretName}]
    }
} if config.ingressEnabled else None
