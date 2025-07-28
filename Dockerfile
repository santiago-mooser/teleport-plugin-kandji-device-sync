### Build
ARG GO_VERSION=1.23.10
ARG ALPINE_VERSION=3.22

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder

# Dependency
RUN apk add --no-cache git gcc g++ make

# Install dependencies
WORKDIR /src
COPY go.mod .
COPY go.sum .
RUN go mod download

# Compile app with rest of source
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 go build

FROM alpine:${ALPINE_VERSION}

ARG GIT_COMMIT_TAG=unspecified \
    GIT_COMMIT_MESSAGE=unspecified \
    GIT_COMMIT_SHA=unspecified \
    IMAGE_BUILD_TIME=unspecified \
    CI_PROJECT_URL=unspecified \
    GITLAB_USER_NAME=unspecified \
    MERGE_REQUEST_URL=unspecified \
    CI_PROJECT_NAME=unspecified

LABEL go_version=1.23.2 \
    commit_message=$GIT_COMMIT_MESSAGE \
    commit_hash=$GIT_COMMIT_SHA \
    commit_tag=$GIT_COMMIT_TAG \
    image_build_time=$IMAGE_BUILD_TIME \
    ci_project_url=$CI_PROJECT_URL \
    gitlab_user_name=$GITLAB_USER_NAME \
    merge_request_url=$MERGE_REQUEST_URL \
    ci_project_name=$CI_PROJECT_NAME

WORKDIR /app
COPY --from=builder \
     /src/teleport-plugin-kandji-device-sync .

CMD [ "/app/teleport-plugin-kandji-device-sync" ]
