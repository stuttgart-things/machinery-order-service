"""
Configuration from the command-line options, and the common labels
"""

import schema

_b = lambda v -> bool {
    v in [True, "True", "true", "1", "yes"]
}

_image = option("config.image")
_namespace = option("config.namespace")
_replicas = option("config.replicas")
_claimApiUrl = option("config.claimApiUrl")
_githubApiUrl = option("config.githubApiUrl")
_basePath = option("config.basePath")
_logFormat = option("config.logFormat")
_debug = option("config.debug")
_installCRDs = option("config.installCRDs")
_ingressEnabled = option("config.ingressEnabled")
_ingressHost = option("config.ingressHost")
_ingressTlsEnabled = option("config.ingressTlsEnabled")
_ingressAnnotations = option("config.ingressAnnotations") or {}
_httpRouteEnabled = option("config.httpRouteEnabled")
_httpRouteParentRefName = option("config.httpRouteParentRefName")
_httpRouteParentRefNamespace = option("config.httpRouteParentRefNamespace")
_httpRouteHostname = option("config.httpRouteHostname")
_httpRouteAnnotations = option("config.httpRouteAnnotations") or {}
_authOidcIssuer = option("config.authOidcIssuer")
_authOidcAudience = option("config.authOidcAudience")
_authOrderRoles = option("config.authOrderRoles")
_authAdminRoles = option("config.authAdminRoles")
_authRoleGroups = option("config.authRoleGroups")
_authTrustForwardedUser = option("config.authTrustForwardedUser")
_githubAppId = option("config.githubAppId")
_githubAppInstallationId = option("config.githubAppInstallationId")
_githubAppSecretName = option("config.githubAppSecretName")
_existingSecretName = option("config.existingSecretName")
_secrets = option("config.secrets") or {}
_extraEnvVars = option("config.extraEnvVars") or {}
_managementClusters = option("config.managementClusters") or {}

config: schema.MachineryOrderService = schema.MachineryOrderService {
    if _image:
        image = _image
    if _namespace:
        namespace = _namespace
    if _replicas:
        replicas = int(_replicas)
    if _claimApiUrl:
        claimApiUrl = _claimApiUrl
    if _githubApiUrl:
        githubApiUrl = _githubApiUrl
    if _basePath:
        basePath = _basePath
    if _logFormat:
        logFormat = _logFormat
    if _debug:
        debug = _b(_debug)
    if _installCRDs != None:
        installCRDs = _b(_installCRDs)
    if _ingressEnabled:
        ingressEnabled = _b(_ingressEnabled)
    if _ingressHost:
        ingressHost = _ingressHost
    if _ingressTlsEnabled:
        ingressTlsEnabled = _b(_ingressTlsEnabled)
    if _ingressAnnotations:
        ingressAnnotations = _ingressAnnotations
    if _httpRouteEnabled:
        httpRouteEnabled = _b(_httpRouteEnabled)
    if _httpRouteParentRefName:
        httpRouteParentRefName = _httpRouteParentRefName
    if _httpRouteParentRefNamespace:
        httpRouteParentRefNamespace = _httpRouteParentRefNamespace
    if _httpRouteHostname:
        httpRouteHostname = _httpRouteHostname
    if _httpRouteAnnotations:
        httpRouteAnnotations = _httpRouteAnnotations
    if _authOidcIssuer:
        authOidcIssuer = _authOidcIssuer
    if _authOidcAudience:
        authOidcAudience = _authOidcAudience
    if _authOrderRoles:
        authOrderRoles = _authOrderRoles
    if _authAdminRoles:
        authAdminRoles = _authAdminRoles
    if _authRoleGroups:
        authRoleGroups = _authRoleGroups
    if _authTrustForwardedUser:
        authTrustForwardedUser = _b(_authTrustForwardedUser)
    if _githubAppId:
        githubAppId = str(_githubAppId)
    if _githubAppInstallationId:
        githubAppInstallationId = str(_githubAppInstallationId)
    if _githubAppSecretName:
        githubAppSecretName = _githubAppSecretName
    if _existingSecretName:
        existingSecretName = _existingSecretName
    if _secrets:
        secrets = _secrets
    if _extraEnvVars:
        extraEnvVars = _extraEnvVars
    if _managementClusters:
        managementClusters = _managementClusters
}

commonLabels = {
    "app.kubernetes.io/name": config.name
    "app.kubernetes.io/instance": config.name
    "app.kubernetes.io/component": "controller"
    "app.kubernetes.io/part-of": "machinery"
    **config.labels
}

selectorLabels = {
    "app.kubernetes.io/name": config.name
    "app.kubernetes.io/instance": config.name
}
