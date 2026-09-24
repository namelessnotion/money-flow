# frozen_string_literal: true
# typed: strict

module Services
  module MoneyFlow
    # Somewhere money moves between: an entity, a Security, or the Bank — the
    # world outside the platform, where ACH deposits come from and withdrawals
    # go.
    class Party < T::Struct
      # What a Party is. An entity's is its Role, by the same serialized value.
      class Kind < T::Enum
        enums do
          Investor = new('investor')
          Borrower = new('borrower')
          Issuer = new('issuer')
          Security = new('security')
          Bank = new('bank')
        end
      end

      const :id, String
      const :kind, Kind
      const :label, String

      sig { params(entity_id: Integer).returns(String) }
      def self.entity_id(entity_id) = "entity:#{entity_id}"

      sig { params(security_id: String).returns(String) }
      def self.security_id(security_id) = "security:#{security_id}"
    end

    BANK = T.let(Party.new(id: 'bank', kind: Party::Kind::Bank, label: 'Bank'), Party)
  end
end
