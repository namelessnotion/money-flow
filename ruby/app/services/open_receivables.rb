# frozen_string_literal: true
# typed: strict

require_relative '../../gen/proto/holder/v1/holder_pb'
require_relative '../../gen/proto/holder/v1/holder_twirp'
require_relative 'det_id'
require_relative 'twirp_call'

module Services
  # Opens a Receivable for every entity onboarded before there were any
  # (ruby/docs/adr/0011). Until it has one, an entity's late deposit return
  # can't be recorded as owed: Services::Ach::EntityWallets raises
  # MissingAccount.
  #
  # The Wallet's id is derived from the entity's holder, so running this
  # again, after any failure, asks for the same Wallet. Provision is
  # idempotent, including partially: it appends only what the holder is
  # missing, so a retry converges instead of opening a second one.
  class OpenReceivables
    class Refused < StandardError; end

    # What a run did.
    class Result < T::Struct
      const :opened, T::Array[Integer]
      # Entity id => the failure, for each entity that still has none.
      const :failed, T::Hash[Integer, String]
    end

    RECEIVABLE = Types::Enums::AccountType::Receivable

    sig { params(holder_client: Holder::V1::HolderServiceClient).void }
    def initialize(
      holder_client: Holder::V1::HolderServiceClient.new(
        ENV.fetch('HOLDER_SERVICE_URL', 'http://localhost:8080/twirp')
      )
    )
      @holder_client = holder_client
    end

    sig { returns(Result) }
    def call
      opened = T.let([], T::Array[Integer])
      failed = T.let({}, T::Hash[Integer, String])
      entities_without_one.each do |entity|
        open_for(entity)
        opened << entity.id
      rescue Refused => e
        failed[entity.id] = "#{e.class}: #{e.message}"
      end
      Result.new(opened: opened, failed: failed)
    end

    private

    T::Sig::WithoutRuntime.sig { returns(T::Array[Models::Entity]) }
    def entities_without_one
      Models::Entity.exclude(
        id: Models::Account.where(type: RECEIVABLE.serialize).select(:entity_id)
      ).order(:id).all
    end

    sig { params(entity: Models::Entity).void }
    def open_for(entity)
      wallet_uuid = DetId.for("#{entity.holder_uuid}:receivable")
      provision!(entity.holder_uuid, wallet_uuid)
      Models::Account.create(entity_id: entity.id, name: RECEIVABLE.serialize, type: RECEIVABLE.serialize,
                             wallet_uuid: wallet_uuid)
    end

    # #provision is defined by protoc-gen-twirp_ruby's `rpc` DSL at load time,
    # which Sorbet can't see; see OnboardEntity#provision!.
    sig { params(holder_uuid: String, wallet_uuid: String).void }
    def provision!(holder_uuid, wallet_uuid)
      request = Holder::V1::ProvisionRequest.new(
        id: holder_uuid,
        wallets: [Holder::V1::WalletSpec.new(wallet_id: wallet_uuid, name: RECEIVABLE.serialize,
                                             allows: RECEIVABLE.allows)]
      )
      response = TwirpCall.with_retries(failure: Refused) { T.unsafe(@holder_client).provision(request) }
      rejected = response.data&.holder_provision_rejected
      raise Refused, rejected.reason if rejected
    end
  end
end
