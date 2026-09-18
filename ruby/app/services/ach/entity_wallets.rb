# frozen_string_literal: true
# typed: strict

require_relative 'errors'

module Services
  module Ach
    # Which Wallet backs each of an entity's accounts, by account type — how an
    # ACH shape turns "from bank to cash" into the wallet ids Go moves money
    # between.
    module EntityWallets
      AccountType = Types::Enums::AccountType

      # The wallet backing each of the `needed` account types. Raises
      # MissingAccount naming every type the entity has no account for.
      sig do
        params(needed: T::Array[AccountType], accounts: T::Array[Models::Account])
          .returns(T::Hash[AccountType, String])
      end
      def self.for(needed, accounts)
        by_type = accounts.to_h { |account| [AccountType.deserialize(account.type), account.wallet_uuid] }
        missing = needed.reject { |type| by_type.key?(type) }
        raise MissingAccount, "entity has no #{missing.map(&:serialize).join(', ')} account" unless missing.empty?

        needed.to_h { |type| [type, by_type.fetch(type)] }
      end
    end
  end
end
