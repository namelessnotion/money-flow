# frozen_string_literal: true
# typed: strict

module Services
  module MoneyFlow
    # One completed Transaction's money, from one Party to another. A
    # Disbursement is two — the principal it returns and the interest it pays —
    # because those are different things to the Investor receiving them.
    class Movement < T::Struct
      # What moved the money. Every kind has exactly one entity on one end; the
      # kind decides what is on the other and which way the money ran.
      class Kind < T::Enum
        enums do
          Deposit = new('deposit')                                # Bank → entity
          Withdrawal = new('withdrawal')                          # entity → Bank
          Subscription = new('subscription')                      # Investor → Security
          Draw = new('draw')                                      # Security → Borrower
          Repayment = new('repayment')                            # Borrower → Security
          DisbursementPrincipal = new('disbursement_principal')   # Security → Investor
          DisbursementInterest = new('disbursement_interest')     # Security → Investor
        end

        # Whether the money ran towards the entity, rather than away from it.
        sig { returns(T::Boolean) }
        def into_entity?
          [Deposit, Draw, DisbursementPrincipal, DisbursementInterest].include?(self)
        end

        # Whether the other end is the Bank, rather than a Security.
        sig { returns(T::Boolean) }
        def across_bank_boundary?
          [Deposit, Withdrawal].include?(self)
        end
      end

      const :kind, Kind
      const :source, String
      const :target, String
      const :amount_minor_units, Integer
      # When Go says the Transaction completed.
      const :occurred_at, Time
      const :transaction_id, String
    end
  end
end
