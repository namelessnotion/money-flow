# frozen_string_literal: true
# typed: strict

# A compressed-time run of a Groundfloor-like lending market through the real
# stack: Investors deposit, buy into Securities, the money is drawn to
# Borrowers, and Borrowers pay it back with interest to be disbursed to the
# Investors — every step a real Go Transaction originated through the same
# services GraphQL uses. Run by bin/simulate_lending.
#
# Application-layer orchestration, not domain: it decides *when* things happen
# and never *how*. Interest, allocation, clearing and every ledger rule stay
# with the services it calls.
module LendingSimulation
end

require_relative 'lending_simulation/quantiles'
require_relative 'lending_simulation/profile'
require_relative 'lending_simulation/book'
require_relative 'lending_simulation/auto_invest'
require_relative 'lending_simulation/money'
require_relative 'lending_simulation/names'
require_relative 'lending_simulation/await'
require_relative 'lending_simulation/platform'
require_relative 'lending_simulation/world'
require_relative 'lending_simulation/investors'
require_relative 'lending_simulation/lending'
require_relative 'lending_simulation/market'
require_relative 'lending_simulation/report'
