# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::OpenReceivables do
  subject(:service) { described_class.new(holder_client: holder_client) }

  # A real client, never connected to: see onboard_entity_spec for why.
  let(:holder_client) { Holder::V1::HolderServiceClient.new('http://holder.internal.test') }

  def provisioned
    Twirp::ClientResp.new(data: Holder::V1::ProvisionResponse.new)
  end

  # An entity onboarded before receivables existed: every account its role
  # opens, except that one.
  def entity_without_receivable(role: Types::Enums::EntityRole::Investor)
    entity = create_provisioned_entity(role: role)
    Models::Account.where(entity_id: entity.id, type: 'receivable').delete
    entity
  end

  def receivable_of(entity)
    Models::Account.first(entity_id: entity.id, type: 'receivable')
  end

  before { allow(holder_client).to receive(:provision).and_return(provisioned) }

  it "opens a receivable on the entity's holder, under an id derived from it" do
    entity = entity_without_receivable

    service.call

    wallet_uuid = Services::DetId.for("#{entity.holder_uuid}:receivable")
    expect(holder_client).to have_received(:provision) { |request|
      expect(request.id).to eq(entity.holder_uuid)
      expect(request.wallets.map(&:to_h)).to eq(
        [{ wallet_id: wallet_uuid, name: 'receivable', allows: :ALLOWS_ONRAMP_AND_OFFRAMP }]
      )
    }
    expect(receivable_of(entity).wallet_uuid).to eq(wallet_uuid)
  end

  it 'leaves an entity that already has one alone' do
    entity = create_provisioned_entity

    expect { service.call }.not_to(change { receivable_of(entity).wallet_uuid })
    expect(holder_client).not_to have_received(:provision)
  end

  it 'names every entity it opened one for' do
    entity = entity_without_receivable

    expect(service.call.opened).to include(entity.id)
  end

  context 'when the holder refuses one' do
    before do
      refused = Twirp::ClientResp.new(
        data: Holder::V1::ProvisionResponse.new(
          holder_provision_rejected: Holder::V1::HolderProvisionRejected.new(reason: 'no such holder')
        )
      )
      allow(holder_client).to receive(:provision).and_return(refused, provisioned)
    end

    it 'records the failure, writes nothing for that entity, and carries on with the rest' do
      first = entity_without_receivable
      second = entity_without_receivable

      result = service.call

      expect(result.failed.keys).to eq([first.id])
      expect(result.failed.fetch(first.id)).to include('no such holder')
      expect(receivable_of(first)).to be_nil
      expect(result.opened).to include(second.id)
    end

    it 'converges on the same wallet when run again' do
      entity = entity_without_receivable
      service.call

      service.call

      expect(receivable_of(entity).wallet_uuid).to eq(Services::DetId.for("#{entity.holder_uuid}:receivable"))
    end
  end
end
