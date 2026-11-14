"""
Non-secret environment of machinery-order-service
"""

import labels

config = labels.config

_env = {
    PORT: str(config.containerPort)
    METRICS_ADDR: ":{}".format(config.metricsPort)
    PROBE_ADDR: ":{}".format(config.probePort)
    ORDER_NAMESPACE: config.namespace
    CLAIM_API_URL: config.claimApiUrl
    LOG_FORMAT: config.logFormat
    LEADER_ELECT: "true" if config.leaderElect else "false"
    if config.debug:
        DEBUG: "true"
    if config.basePath:
        BASE_PATH: config.basePath
    if config.githubApiUrl:
        GITHUB_API_URL: config.githubApiUrl
    if config.prPollInterval:
        PR_POLL_INTERVAL: config.prPollInterval
    if config.livePollInterval:
        LIVE_POLL_INTERVAL: config.livePollInterval
    if config.authOidcIssuer:
        AUTH_OIDC_ISSUER: config.authOidcIssuer
        AUTH_OIDC_AUDIENCE: config.authOidcAudience
    if config.authOrderRoles:
        AUTH_ORDER_ROLES: config.authOrderRoles
    if config.authAdminRoles:
        AUTH_ADMIN_ROLES: config.authAdminRoles
    if config.authRoleGroups:
        AUTH_ROLE_GROUPS: config.authRoleGroups
    if config.authTrustForwardedUser:
        AUTH_TRUST_FORWARDED_USER: "true"
    if config.githubAppId:
        GITHUB_APP_ID: config.githubAppId
        GITHUB_APP_INSTALLATION_ID: config.githubAppInstallationId
        GITHUB_APP_PRIVATE_KEY_FILE: "/etc/github-app/private-key.pem"
    **config.extraEnvVars
}

configMap = {
    apiVersion: "v1"
    kind: "ConfigMap"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
    }
    data: _env
}
