"""
Deployment of machinery-order-service
"""

import labels

config = labels.config

_secretRefs = ([{secretRef.name: config.name}] if config.secrets else []) + ([{secretRef.name: config.existingSecretName}] if config.existingSecretName else [])

deployment = {
    apiVersion: "apps/v1"
    kind: "Deployment"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
        if config.annotations:
            annotations: config.annotations
    }
    spec: {
        replicas: config.replicas
        selector.matchLabels: labels.selectorLabels
        template: {
            metadata: {
                labels: labels.commonLabels
                if config.annotations:
                    annotations: config.annotations
            }
            spec: {
                serviceAccountName: config.name
                containers: [{
                    name: config.name
                    image: config.image
                    imagePullPolicy: config.imagePullPolicy
                    args: ["serve"]
                    ports: [
                        {name: "http", containerPort: config.containerPort, protocol: "TCP"}
                        {name: "metrics", containerPort: config.metricsPort, protocol: "TCP"}
                        {name: "probes", containerPort: config.probePort, protocol: "TCP"}
                    ]
                    env: [{
                        name: "POD_NAMESPACE"
                        valueFrom.fieldRef.fieldPath: "metadata.namespace"
                    }]
                    envFrom: [{configMapRef.name: config.name}] + _secretRefs
                    resources: {
                        requests: {cpu: config.cpuRequest, memory: config.memoryRequest}
                        limits: {cpu: config.cpuLimit, memory: config.memoryLimit}
                    }
                    livenessProbe: {
                        httpGet: {path: "/healthz", port: "probes"}
                        initialDelaySeconds: 10
                        periodSeconds: 15
                    }
                    readinessProbe: {
                        httpGet: {path: "/ready", port: "http"}
                        initialDelaySeconds: 5
                        periodSeconds: 10
                    }
                    securityContext: {
                        readOnlyRootFilesystem: True
                        runAsNonRoot: True
                        allowPrivilegeEscalation: False
                        capabilities.drop: ["ALL"]
                    }
                    volumeMounts: [{name: "tmp", mountPath: "/tmp"}] + ([{
                        name: "github-app"
                        mountPath: "/etc/github-app"
                        readOnly: True
                    }] if config.githubAppSecretName else [])
                }]
                securityContext: {
                    runAsNonRoot: True
                    runAsUser: 65532
                    runAsGroup: 65532
                    fsGroup: 65532
                    seccompProfile.type: "RuntimeDefault"
                }
                volumes: [{name: "tmp", emptyDir: {}}] + ([{
                    name: "github-app"
                    secret: {
                        secretName: config.githubAppSecretName
                        items: [{key: "private-key.pem", path: "private-key.pem"}]
                    }
                }] if config.githubAppSecretName else [])
            }
        }
    }
}
