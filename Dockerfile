# Pinned by digest so the runtime layer is reproducible; Dependabot bumps it.
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG TARGETOS
ARG TARGETARCH
COPY bin/manager-${TARGETOS}-${TARGETARCH} /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
