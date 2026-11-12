# Pinned by digest so the runtime layer is reproducible; Dependabot bumps it.
FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
ARG TARGETOS
ARG TARGETARCH
COPY bin/manager-${TARGETOS}-${TARGETARCH} /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
