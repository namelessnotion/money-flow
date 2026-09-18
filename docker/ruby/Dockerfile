# Build context is the repo root (see docker-compose.yml) so this can also
# COPY the top-level proto-generated dirs if that's ever needed; for now it
# only needs ruby/.
FROM ruby:3.4.10-slim

# procps: Resque workers shell out to `ps` to find and prune dead workers.
RUN apt-get update -qq \
  && apt-get install -y --no-install-recommends build-essential libpq-dev libyaml-dev postgresql-client git procps \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY ruby/Gemfile ruby/Gemfile.lock ./
RUN bundle install

COPY ruby/ .

COPY docker/ruby/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 9292

ENTRYPOINT ["/entrypoint.sh"]
CMD ["bin/server"]
