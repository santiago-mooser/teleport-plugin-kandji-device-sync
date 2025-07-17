# Teleport-kandji-device-sync

This helm chart is used to deploy the service **without** any secrets. You will still need to deploy the following secrets in the namespace:

1. `KANDJI_API_TOKEN` See [[1]](https://github.com/santiago-mooser/teleport-plugin-kandji-device-syncer/blob/add-helm-chart/helm/templates/deployment.yaml#L41-L45)
   ```yaml
   name: {{ include "kandji-cloudflare-syncer.fullname" . }}-secrets
   key: KANDJI_API_TOKEN
   ```
1. `TELEPORT_IDENTITY_FILE` see [[2]](https://github.com/santiago-mooser/teleport-plugin-kandji-device-syncer/blob/add-helm-chart/helm/templates/deployment.yaml#L64-L69)
   ```yaml
   - key: auth_id
     path: identity.pem
   ```
   
