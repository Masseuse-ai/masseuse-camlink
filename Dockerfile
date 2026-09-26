# Built by goreleaser (dockers_v2): the build context holds the release
# binaries under <os>/<arch>/, one image for every platform.
#
# gcr.io/distroless/static-debian12:nonroot, pinned by digest: no shell, no
# package manager, an unprivileged user (65532).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETPLATFORM

# The state directory (identity key and pairings) must be owned by the
# unprivileged user so a fresh named volume mounted here is writable.
COPY --from=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab --chown=65532:65532 /home/nonroot /state
COPY $TARGETPLATFORM/masseuse-camlink /usr/bin/masseuse-camlink

ENV MASSEUSE_CAMLINK_STATE_DIR=/state
VOLUME /state
USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/masseuse-camlink"]
