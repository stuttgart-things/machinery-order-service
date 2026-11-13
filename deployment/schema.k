"""
Schema of the machinery-order-service deployment configuration
"""

import regex

schema MachineryOrderService:
    """Configuration of the machinery-order-service deployment"""

    name: str = "machinery-order-service"
    namespace: str = "machinery-order-service"

    image: str = "ghcr.io/stuttgart-things/machinery-order-service:latest"
    imagePullPolicy: str = "IfNotPresent"

    # The controller is leader-elected; the API answers on every replica.
    replicas: int = 1
    leaderElect: bool = True
    cpuRequest: str = "50m"
    cpuLimit: str = "500m"
    memoryRequest: str = "64Mi"
    memoryLimit: str = "256Mi"

    serviceType: str = "ClusterIP"
    servicePort: int = 8080
    containerPort: int = 8080
    metricsPort: int = 8081
    probePort: int = 8082

    # CRDs and the ClusterRole (deployment/generated, from config/ by
    # `task generate`). Off when the CRDs are installed some other way.
    installCRDs: bool = True

    # Ingress
    ingressEnabled: bool = False
    ingressClassName: str = "nginx"
    ingressHost: str = "machinery-order-service.example.com"
    ingressTlsEnabled: bool = False
    ingressTlsSecretName: str = "machinery-order-service-tls"
    ingressAnnotations: {str:str} = {}

    # HTTPRoute (Gateway API)
    httpRouteEnabled: bool = False
    httpRouteParentRefName: str = ""
    httpRouteParentRefNamespace: str = ""
    httpRouteHostname: str = ""
    httpRouteAnnotations: {str:str} = {}

    # Application
    claimApiUrl: str = ""
    githubApiUrl: str = ""
    # Prefix of UI and API behind sthings-portal, e.g. "/orders" (BASE_PATH).
    basePath: str = ""
    logFormat: str = "json"
    debug: bool = False
    prPollInterval: str = ""
    livePollInterval: str = ""

    # Auth (ZITADEL), as in claim-machinery-api; empty issuer = orders open.
    authOidcIssuer: str = ""
    authOidcAudience: str = ""
    authOrderRoles: str = ""
    authAdminRoles: str = ""
    authRoleGroups: str = ""
    authTrustForwardedUser: bool = False

    # GitHub App (the bot identity): ids here, the private key from a Secret
    # with the key private-key.pem, mounted as a file.
    githubAppId: str = ""
    githubAppInstallationId: str = ""
    githubAppSecretName: str = ""

    # Environment from an existing Secret (GITHUB_TOKEN, CLAIM_API_TOKEN,
    # CLAIM_API_OIDC_CLIENT_SECRET, ...), e.g. made by External Secrets.
    existingSecretName: str = ""
    # Or a Secret made here (values base64 encoded); for tests only.
    secrets: {str:str} = {}

    extraEnvVars: {str:str} = {}

    # ManagementCluster objects to create (their spec), by name.
    managementClusters: {str:any} = {}

    serviceAccountAnnotations: {str:str} = {}
    labels: {str:str} = {}
    annotations: {str:str} = {}

    check:
        replicas >= 0, "replicas must be >= 0"
        servicePort >= 1 and servicePort <= 65535, "servicePort must be valid"
        containerPort >= 1 and containerPort <= 65535, "containerPort must be valid"
        claimApiUrl, "claimApiUrl is required"
        not basePath or regex.match(basePath, r"^(/[A-Za-z0-9._~-]+)+$"), "basePath must be empty or /segment[/segment...] without a trailing slash"
        not httpRouteEnabled or httpRouteParentRefName, "httpRouteParentRefName is required with httpRouteEnabled"
        (githubAppId and githubAppInstallationId and githubAppSecretName) or (not githubAppId and not githubAppInstallationId and not githubAppSecretName), "githubAppId, githubAppInstallationId and githubAppSecretName go together"
        not authOidcIssuer or authOidcAudience, "authOidcAudience is required with authOidcIssuer"
