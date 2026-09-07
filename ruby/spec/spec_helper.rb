# frozen_string_literal: true

# Must be set before lib/environment (and through it lib/boot) loads: it is what
# points DB at the test database instead of the development one.
ENV['APP_ENV'] ||= 'test'

require_relative '../lib/environment'
require 'factory_bot'

# Specs run inside a rollback transaction, so they are isolated from each other
# — but not from rows already committed by using the app. Sharing a database
# with development therefore breaks any spec that queries a whole table, and it
# puts real local data within reach of a stray commit. Fail loudly rather than
# subtly: the connected database's name must say it is the test one.
unless DB.opts[:database].to_s.end_with?('_test')
  raise "specs must run against a database whose name ends in _test, got #{DB.opts[:database].inspect}. " \
        'Set TEST_DATABASE_URL, and run bin/setup_test_db to create and migrate it.'
end

Dir[File.join(__dir__, 'factories', '**', '*.rb')].each { |file| require file }

RSpec.configure do |config|
  config.include FactoryBot::Syntax::Methods

  config.expect_with :rspec do |expectations|
    expectations.include_chain_clauses_in_custom_matcher_descriptions = true
  end

  config.mock_with :rspec do |mocks|
    mocks.verify_partial_doubles = true
  end

  config.shared_context_metadata_behavior = :apply_to_host_groups

  config.around do |example|
    DB.transaction(rollback: :always) { example.run }
  end

  config.filter_run_when_matching :focus
  config.warnings = true
  config.order = :random
  Kernel.srand config.seed
end
