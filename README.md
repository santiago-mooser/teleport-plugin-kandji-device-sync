# Unofficial Teleport Kandji Device Trust Integration

This Go service continuously syncs devices from a Kandji instance into a Teleport cluster's Device Trust inventory using the Teleport API client. It supports filtering by blueprint ID, name and device type (mobile/not mobile). For more information, please check [the `config.example.yaml` file](./config.example.yaml).

## Disclaimer
This is an unofficial integration and is not supported by either Kandji or Teleport. Use at your own risk. Ensure you have backups and understand the implications of syncing devices between these two systems.
The software is provided as-is without any warranties or guarantees. By using this software, you agree to take full responsibility for any issues that may arise from its use. Please see LICENSE for more details.

## Todo

- [x] Refresh the identity file on sync in order to support tbot-based identity files

## How it Works

The service performs a one-way synchronization:
1.  Fetches all devices from the Kandji API.
2.  Filters out devices based on the filters specified in the config file.
3.  Fetches all currently trusted devices from the Teleport cluster using the Teleport API client.
4.  Compares the two lists based on device serial numbers and checks:
    a) Wich devices are in teleport but not in Kandji (after filtering)
    b) Which devices are in Kandji but not in teleport   
6.  If a device exists in Kandji but not in Teleport, it is added to Teleport's trusted devices using the API client.
7.  (If enabled) If a device exists in Teleport but not Kandji, it is deleted from Teleport's trusted devices list.

## Prerequisites

- **Go**: Version 1.23.10 or later (if compiling and not building dockerfile).
- **Kandji API Key**: You need an API key from your Kandji instance with permissions to read device information.
- **A method to authenticate with teleport**:
  - **Option 1: Deploy Teleport's tbot** (recommended): The tbot should automatically deploy a secret to the namespace you deploy it to once you configure it correctly. Either point the helm chart to mount the secret (check the secret name) or specify the file using the `TELEPORT_IDENTITY_FILE` environment variable.
  - **Option 2: Static Teleport Identity file**: An identity file (`.pem`) for a Teleport user with a role that has the following permissions (disclaimer: this will use one of your licenses):
    ```yaml
    kind: role
    version: v7
    metadata:
      name: kandji-syncer
    spec:
      allow:
        rules:
          - resources: ['device']
            verbs: ['list', 'create', 'read']
    ```
    You can generate the certificate using:
    ```bash
    tctl auth sign --user=kandji-syncer --out=identity --ttl 365d
    ```
    You might need to create a role that lets you impersonate the role to be able to generate the identity file:
    ```yaml
    kind: role
    version: v7
    metadata:
      name: kandji-syncer-impersonator
    spec:
      allow:
        impersonate:
          roles:
          - kandji-syncer
          users:
          - kandji-syncer
    ```

## Building from source

1.  **Clone the Repository:**
    ```bash
    git clone <your-repo-url>
    cd teleport-plugin-kandji-device-sync
    ```

2.  **Initialize Go modules:**
    ```bash
    go mod init teleport-plugin-kandji-device-sync
    go mod tidy
    ```

3.  **Configure:**
    Copy the example configuration file:
    ```bash
    cp config.yaml.example config.yaml
    ```
    Edit `config.yaml` with your Kandji and Teleport details. Additionally, can set the following environment variables:
    -   `KANDJI_API_URL`
    -   `KANDJI_API_TOKEN`
    -   `TELEPORT_PROXY_ADDR`
    -   `TELEPORT_IDENTITY_FILE`

4.  **Build the application:**
    ```bash
    go build -o teleport-plugin-kandji-device-sync .
    ```

5.  **Run the service:**
    ```bash
    ./teleport-plugin-kandji-device-sync
    ```
    The service will start logging its output to standard out in JSON format.

## Running in Docker

You can run the service in a Docker container for easier deployment:

```bash
docker buildx build -t teleport-plugin-kandji-device-sync .
```

Then you can run the Docker container with the required environment variables:

```bash
docker run \
-e KANDJI_API_URL=https://EXAMPLE.api.kandji.io \
-e KANDJI_API_TOKEN=REPLACExx-MExx-NOTx-SAFE-TOxUSExxxxxx \
-e TELEPORT_PROXY_ADDR="EXAMPLE.teleport.sh:443" \
-e TELEPORT_IDENTITY_FILE="identity.pem" \
-v "${PWD}/identity.pem:/app/identity.pem" \
teleport-plugin-kandji-device-sync
```


## (Advanced) Deploying in Kubernetes 

A working helm chart exists in [the `./helm` directory](./helm), but you will need to deploy the secret yourself. Please check out Teleport's [Deploying Machine ID on Kubernetes](https://goteleport.com/docs/machine-workload-identity/machine-id/deployment/kubernetes/) article on how to deploy Teleport's Tbot.
