import labels

config = labels.config

grpcroute = {
    apiVersion: "gateway.networking.k8s.io/v1"
    kind: "GRPCRoute"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
        if config.grpcRouteAnnotations:
            annotations: config.grpcRouteAnnotations
    }
    spec: {
        parentRefs: [{
            name: config.httpRouteParentRefName
            if config.httpRouteParentRefNamespace:
                namespace: config.httpRouteParentRefNamespace
        }]
        hostnames: [config.grpcRouteHostname]
        rules: [{
            backendRefs: [{name: config.name, port: config.grpcPort}]
        }]
    }
} if config.grpcRouteEnabled else None
