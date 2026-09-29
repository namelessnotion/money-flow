# frozen_string_literal: true
# typed: false

require_relative 'boot'

# Eager-loads every application file. `Sequel::Model.inherited` issues a real schema
# query when a model class is defined, so this needs a live, migrated database — it
# isn't a pure load step.
#
# Used by spec/spec_helper.rb and by sorbet/tapioca/extensions/load_app.rb, which is
# how the tapioca-sequel compiler gets to see the models (`tapioca dsl` only knows
# how to boot Rails apps).
module Environment
  ROOT = File.expand_path('..', __dir__)

  # Ordering is cosmetic — every file require_relatives its own dependencies — but
  # keeping models ahead of their consumers matches how the app is layered.
  LAYERS = %w[types models services consumer jobs graphql].freeze

  def self.load_app!
    LAYERS.each do |layer|
      Dir[File.join(ROOT, 'app', layer, '**', '*.rb')].each { |file| require file }
    end
  end
end

Environment.load_app!
