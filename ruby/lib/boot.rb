# frozen_string_literal: true
# typed: false

require 'sequel'
require 'graphql'
require_relative 'core_ext/sorbet_sig'

# Development and test must never share a database. The suite asserts on
# whole-table queries ("these are all the entities"), so a single row left
# behind by using the app locally is enough to fail it — which is exactly what
# happened before this split existed. APP_ENV=test, set by spec/spec_helper.rb
# before this file loads, selects TEST_DATABASE_URL instead.
DATABASE_URL =
  if ENV['APP_ENV'] == 'test'
    ENV.fetch('TEST_DATABASE_URL', 'postgres://money_flow:money_flow@localhost:5432/money_flow_test?sslmode=disable')
  else
    ENV.fetch('DATABASE_URL', 'postgres://money_flow:money_flow@localhost:5432/money_flow_dev?sslmode=disable')
  end

DB = Sequel.connect(DATABASE_URL)

# Generated protobuf/twirp code (e.g. `require 'holder/v1/holder_pb'`) lives
# under gen/proto rather than lib, so it isn't on the load path by default.
$LOAD_PATH.unshift(File.expand_path('../gen/proto', __dir__))
