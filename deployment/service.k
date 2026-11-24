import labels

config = labels.config

service = {
    apiVersion: "v1"
    kind: "Service"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
    }
    spec: {
        type: config.serviceType
        selector: labels.selectorLabels
        ports: [
            {name: "http", port: config.servicePort, targetPort: "http", protocol: "TCP"}
            {name: "grpc", port: config.grpcPort, targetPort: "grpc", protocol: "TCP", appProtocol: "kubernetes.io/h2c"}
            {name: "metrics", port: config.metricsPort, targetPort: "metrics", protocol: "TCP"}
        ]
    }
}
