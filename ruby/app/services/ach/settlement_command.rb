# frozen_string_literal: true
# typed: strict

require_relative '../base_service'
require_relative 'go_gateway'

module Services
  module Ach
    # What Settle and Return share: both are the provider reporting the fate of
    # an entry already submitted, and both end by resuming the Transaction so
    # Go acts on it.
    class SettlementCommand < BaseService
      sig { params(gateway: GoGateway).void }
      def initialize(gateway: GoGateway.new)
        super()
        @go = gateway
      end

      private

      sig { params(id: String).returns(Models::AchTransaction) }
      def find!(id)
        Models::AchTransaction[id] || raise(NotFound, "no ACH transaction #{id}")
      end
    end
  end
end
