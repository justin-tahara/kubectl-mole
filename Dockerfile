# Runtime image only: goreleaser builds the binaries and lays them out in the
# context per platform (dockers_v2 contract).
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/kubectl-mole /usr/local/bin/kubectl-mole
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/kubectl-mole"]
