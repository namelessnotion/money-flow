# frozen_string_literal: true
# typed: strict

require 'securerandom'
require_relative '../base_service'
require_relative '../det_id'
require_relative 'errors'
require_relative 'go_gateway'
require_relative 'offering_shape'

module Services
  module Securities
    # Opens a Security for subscription: its four Wallets, its row, and the
    # Transaction that mints its Supply.
    #
    # 1. Validates the parties and the terms, and chooses the Security's id —
    #    every other id it needs is derived from that one, so they are all
    #    known before anything is written.
    # 2. Provisions the four Wallets on the Issuer's existing Holder, outside
    #    any database transaction, all-or-nothing. A local row must never name
    #    a Wallet that was never opened.
    # 3. Records the Security and its accounts, with the ids it is about to
    #    send.
    # 4. Mints the Supply, which is the only way claims enter the ledger.
    #
    # A crash between 3 and 4 leaves a Security whose Supply was never minted,
    # and that is survivable rather than hidden: every id is derived, so
    # `ensure_open` re-sends exactly the same ones and Go converges. Such a
    # Security shows as `stage: OFFERING` with no offering state at all, which
    # is the "needs a person" signal ADR 0003 already uses.
    #
    # Lifecycle state is not recorded here: it arrives through the projection.
    class IssueOffering < BaseService
      # The terms of an offering.
      class Request < T::Struct
        const :issuer_entity_id, Integer
        const :borrower_entity_id, Integer
        const :name, String
        const :principal_minor_units, Integer
        const :annual_rate_bps, Integer
        const :term_days, Integer
      end

      AccountType = Types::Enums::AccountType
      EntityRole = Types::Enums::EntityRole

      # The Wallets an offering opens. They belong to the Security rather than
      # to any entity, which is why onboarding never opens them. Escrow and
      # repayment hold its cleared cash; SecurityCash holds the real money
      # behind both (ruby/docs/adr/0009).
      WALLETS = T.let(
        [AccountType::SecuritySupply, AccountType::SecurityEscrow, AccountType::SecurityRepayment,
         AccountType::SecurityCash].freeze,
        T::Array[AccountType]
      )

      sig { params(gateway: GoGateway).void }
      def initialize(gateway: GoGateway.new)
        super()
        @go = gateway
      end

      sig { params(request: Request).returns(Models::Security) }
      def call(request:)
        issuer = party!(request.issuer_entity_id, EntityRole::Issuer)
        party!(request.borrower_entity_id, EntityRole::Borrower)
        validate_terms!(request)

        id = SecureRandom.uuid_v7
        plans = WALLETS.to_h { |type| [type, SecureRandom.uuid_v7] }

        provision!(issuer, plans)
        security = perform { record(request, id, plans) }
        mint_supply!(security)
        security
      end

      # Re-sends a Security's Supply mint. Safe at any time: the ids are
      # derived, so Go returns the decision it already recorded rather than
      # minting twice.
      sig { params(security: Models::Security).returns(Models::Security) }
      def ensure_open(security)
        mint_supply!(security)
        security
      end

      private

      sig { params(entity_id: Integer, role: EntityRole).returns(Models::Entity) }
      def party!(entity_id, role)
        entity = Models::Entity[entity_id] || raise(NotFound, "no entity #{entity_id}")
        return entity if entity.role == role.serialize

        raise WrongRole, "entity #{entity_id} is a #{entity.role}, not #{role.serialize}"
      end

      sig { params(request: Request).void }
      def validate_terms!(request)
        raise InvalidAmount, 'principal must be positive' unless request.principal_minor_units.positive?
        raise InvalidAmount, 'rate must not be negative' if request.annual_rate_bps.negative?
        raise InvalidAmount, 'term must be at least a day' unless request.term_days.positive?
      end

      # One all-or-nothing Provision on the Issuer's existing Holder. Go
      # appends only the wallets that are missing, so a retry after an
      # ambiguous failure converges rather than conflicting — which is why the
      # same uuids are reused rather than regenerated.
      sig { params(issuer: Models::Entity, plans: T::Hash[AccountType, String]).void }
      def provision!(issuer, plans)
        specs = plans.map do |type, wallet_uuid|
          Holder::V1::WalletSpec.new(wallet_id: wallet_uuid, name: type.serialize, allows: type.allows)
        end
        @go.provision_wallets(issuer.holder_uuid, specs)
      end

      sig { params(request: Request, id: String, plans: T::Hash[AccountType, String]).returns(Models::Security) }
      def record(request, id, plans)
        security = Models::Security.create(
          id: id, name: request.name,
          issuer_entity_id: request.issuer_entity_id, borrower_entity_id: request.borrower_entity_id,
          principal_minor_units: request.principal_minor_units, annual_rate_bps: request.annual_rate_bps,
          term_days: request.term_days, currency: CURRENCY,
          offering_transaction_id: DetId.for("security:#{id}:offering"),
          offering_transfer_id: DetId.for("security:#{id}:offering:supply")
        )
        open_accounts(security, plans)
        security
      end

      sig { params(security: Models::Security, plans: T::Hash[AccountType, String]).void }
      def open_accounts(security, plans)
        plans.each do |type, wallet_uuid|
          Models::Account.create(
            entity_id: security.issuer_entity_id, security_id: security.id,
            name: type.serialize, type: type.serialize, wallet_uuid: wallet_uuid
          )
        end
      end

      sig { params(security: Models::Security).void }
      def mint_supply!(security)
        transaction_id = required(security, :offering_transaction_id)
        shape = OfferingShape.for(
          security: security,
          accounts: Models::Account.where(entity_id: security.issuer_entity_id).all,
          supply_transfer_id: required(security, :offering_transfer_id)
        )
        @go.start_transaction(
          shape.start_request(transaction_id: transaction_id,
                              amount_minor_units: security.principal_minor_units)
        )
      end

      # The mint ids are nullable on the row — a Security exists for a moment
      # before they are written — but nothing may be sent to Go without them.
      # `ensure_open` can be handed any Security, so this is a real check
      # rather than a narrowing for the typechecker's benefit.
      sig { params(security: Models::Security, column: Symbol).returns(String) }
      def required(security, column)
        security.public_send(column) || raise(NotFound, "security #{security.id} has no #{column}")
      end
    end
  end
end
