FROM ghcr.io/rknightion/cursor-cloud-base:1@sha256:d682eea83896694ce529c2035816610edd723c9acbd39ccaf8056743e7ed1756

USER root
# Tests start their own PostgreSQL 18 clusters. Install the binaries without a system cluster.
RUN set -eu; \
    case "$(dpkg --print-architecture)" in \
        amd64) archive=https://archive.ubuntu.com/ubuntu/ ;; \
        arm64) archive=https://ports.ubuntu.com/ubuntu-ports/ ;; \
        *) exit 1 ;; \
    esac; \
    scratch=$(mktemp -d); \
    policy=/usr/sbin/policy-rc.d; \
    if [ -e "$policy" ] || [ -L "$policy" ]; then mv "$policy" "$scratch/policy-rc.d"; fi; \
    trap 'rm -f "$policy"; if [ -e "$scratch/policy-rc.d" ] || [ -L "$scratch/policy-rc.d" ]; then mv "$scratch/policy-rc.d" "$policy"; fi; rm -rf "$scratch"' EXIT; \
    printf '#!/bin/sh\nexit 101\n' > "$policy"; \
    chmod 0755 "$policy"; \
    key=/usr/share/keyrings/postgresql.asc; \
    curl -q -fsSL --proto '=https' --proto-redir '=https' \
      https://www.postgresql.org/media/keys/ACCC4CF8.asc -o "$key"; \
    printf '%s  %s\n' 0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76 "$key" | sha256sum --check; \
    for suite in noble noble-updates noble-security; do \
        printf 'deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] %s %s main universe\n' "$archive" "$suite"; \
    done > "$scratch/sources.list"; \
    printf 'deb [signed-by=%s] https://apt.postgresql.org/pub/repos/apt noble-pgdg main\n' "$key" >> "$scratch/sources.list"; \
    set -- -o "Dir::Etc::sourcelist=$scratch/sources.list" -o Dir::Etc::sourceparts=-; \
    apt-get "$@" -o APT::Update::Error-Mode=any update; \
    DEBIAN_FRONTEND=noninteractive apt-get "$@" install -y --no-install-recommends postgresql-common; \
    sed -i '/^[[:space:]#]*create_main_cluster[[:space:]]*=/d' /etc/postgresql-common/createcluster.conf; \
    printf '\ncreate_main_cluster = false\n' >> /etc/postgresql-common/createcluster.conf; \
    DEBIAN_FRONTEND=noninteractive apt-get "$@" install -y --no-install-recommends postgresql-18; \
    rm -rf /var/lib/apt/lists/*

ENV PATH="/usr/lib/postgresql/18/bin:${PATH}"
USER ubuntu
