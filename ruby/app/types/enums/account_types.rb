# frozen_string_literal: true
# typed: strict

require_relative 'entity_role'

module Types
  module Enums
    # Different types of accounts that can be created.
    #
    # Every value states its serialized form explicitly. T::Enum would otherwise
    # derive it by downcasing the constant with no separator ("debitcard",
    # "unclearedcash"), and these strings are persisted in accounts.type and
    # sent over the wire as a Wallet's name.
    class AccountType < T::Enum
      extend T::Sig

      enums do
        Bank = new('bank')                     # ACH Bank Account
        BankControl = new('bank_control')      # Bank's platform-side control wallet
        DebitCard = new('debit_card')          # Debit Card Account
        UnclearedCash = new('uncleared_cash')  # Cash waiting to be cleared
        ClearedCash = new('cleared_cash')      # Cash that has cleared
        Cash = new('cash')                     # Cash that can be used for purchases
        Gain = new('gain')                     # Gain account for tracking profits
        Loss = new('loss')                     # Loss account for tracking losses

        # Securities. The first two belong to an entity; the last three belong
        # to a Security and are opened per offering, never by onboarding.
        Investment = new('investment')                  # An Investor's claims
        IssuerControl = new('issuer_control')           # Where an Issuer mints claim supply from
        SecuritySupply = new('security_supply')         # A Security's unsold claims
        SecurityEscrow = new('security_escrow')         # Investor money held until the Draw
        SecurityRepayment = new('security_repayment')   # Borrower money held until disbursement
      end

      # What the backing Wallet may do at the platform boundary: onramp brings
      # money in from outside, offramp sends it out. Only funding instruments
      # touch that boundary — everything else moves money that is already
      # inside the platform, so it permits neither.
      #
      # IssuerControl is the one exception, and it is not about the boundary at
      # all: Go's validateMintSource refuses mint_source from any Wallet
      # narrower than ALLOWS_ONRAMP, and minting supply is the only way claims
      # enter the ledger. Its Token carries no flag as a result, so it may run
      # permanently negative — which is exactly right, because that negative is
      # total claims outstanding, and it returns to zero as claims are retired
      # back into it. The same shape as BankControl.
      #
      # ALLOWS_NONE gives a Token debits_must_not_exceed_credits, and that is
      # what makes SecuritySupply an oversubscription control and Investment a
      # guarantee that no holder has more principal retired than they hold.
      #
      # ALLOWS_NONE rather than ALLOWS_UNSPECIFIED: the Wallet service rejects
      # an unset policy, so "neither direction" has to be said out loud.
      #
      # Returned as the enum's symbol rather than its integer. google-protobuf
      # accepts either and raises RangeError on an unknown name, whereas the
      # generated Shared::V1::Allows::* constants are built at runtime from the
      # descriptor pool and so cannot be resolved statically.
      sig { returns(Symbol) }
      def allows
        case self
        when Bank, BankControl, IssuerControl then :ALLOWS_ONRAMP_AND_OFFRAMP
        when DebitCard then :ALLOWS_ONRAMP
        else :ALLOWS_NONE
        end
      end

      # Every role banks, so every role gets the accounts an ACH shape moves
      # money through (Services::Ach::TransactionShape, ClearingShape). Beyond
      # that a role gets only what its own part in the market uses: an account
      # an entity can never move money through is a Wallet nobody will be able
      # to explain later.
      BANKING = T.let([Bank, BankControl, UnclearedCash, ClearedCash, Cash].freeze, T::Array[AccountType])

      # A Security's own three types appear in no role's list. They are opened
      # per offering by Services::Securities::IssueOffering, and the
      # accounts_security_scoped_types CHECK refuses one with no security_id.
      BY_ROLE = T.let(
        {
          Types::Enums::EntityRole::Investor => [*BANKING, DebitCard, Investment].freeze,
          Types::Enums::EntityRole::Borrower => BANKING,
          Types::Enums::EntityRole::Issuer => [*BANKING, IssuerControl, Gain, Loss].freeze
        }.freeze,
        T::Hash[Types::Enums::EntityRole, T::Array[AccountType]]
      )

      # Which accounts onboarding opens for an entity playing `role`.
      sig { params(role: Types::Enums::EntityRole).returns(T::Array[AccountType]) }
      def self.for_role(role)
        BY_ROLE.fetch(role)
      end
    end
  end
end
