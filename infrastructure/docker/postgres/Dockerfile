FROM postgres:16-alpine

RUN mkdir -p /etc/postgresql \
    && chown postgres:postgres /etc/postgresql \
    && chmod 0750 /etc/postgresql

COPY entrypoint-wrapper.sh configure-ops-metrics-hba.sh /usr/local/bin/
COPY init/00-ops-metrics-hba.sh /docker-entrypoint-initdb.d/
RUN chmod +x \
    /usr/local/bin/entrypoint-wrapper.sh \
    /usr/local/bin/configure-ops-metrics-hba.sh \
    /docker-entrypoint-initdb.d/00-ops-metrics-hba.sh

# The official postgres:16-alpine docker-entrypoint.sh must start as root: on a fresh or
# root-owned named volume it chowns PGDATA to postgres, then drops privileges itself via
# `gosu postgres`. Setting USER postgres before the ENTRYPOINT would break that first-start
# chown and initdb. entrypoint-wrapper.sh runs as root only until it hands off to it.
# nosemgrep: dockerfile.security.missing-user-entrypoint.missing-user-entrypoint
ENTRYPOINT ["entrypoint-wrapper.sh"]
# nosemgrep: dockerfile.security.missing-user.missing-user
CMD ["postgres"]
