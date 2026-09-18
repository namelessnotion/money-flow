# frozen_string_literal: true
# typed: strict

require 'resque'
require 'resque-scheduler'
require 'yaml'
require_relative 'environment'

# Points Resque at Redis and hands resque-scheduler the recurring schedule.
# Run by the `resque:setup` Rake task, which both `rake resque:work` and
# `rake resque:scheduler` invoke before starting.
module ResqueBoot
  SCHEDULE_PATH = T.let(File.expand_path('../config/resque_schedule.yml', __dir__), String)

  sig { void }
  def self.load!
    Resque.redis = redis_url(ENV.to_h)
    Resque.schedule = schedule
  end

  # config/resque_schedule.yml, parsed.
  sig { returns(T::Hash[String, T::Hash[String, String]]) }
  def self.schedule
    T.cast(YAML.safe_load_file(SCHEDULE_PATH), T::Hash[String, T::Hash[String, String]])
  end

  sig { params(env: T::Hash[String, String]).returns(String) }
  def self.redis_url(env)
    env.fetch('REDIS_URL', 'redis://localhost:6379/0')
  end
end
