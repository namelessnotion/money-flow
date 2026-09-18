# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Consumer::BalanceProjector do
  subject(:projector) { described_class.new }

  let(:token_id) { SecureRandom.uuid_v7 }
  let(:wallet_id) { SecureRandom.uuid_v7 }

  def recorded(sequence, posted:, outgoing: 0, incoming: 0)
    message = Token::V1::TokenBalanceRecorded.new(
      id: token_id, wallet_id: wallet_id, currency: 'USD', posted_minor_units: posted,
      pending_outgoing_minor_units: outgoing, pending_incoming_minor_units: incoming
    )
    envelope_for(message, aggregate_id: token_id, sequence: sequence)
  end

  def row
    Models::TokenBalanceProjection[token_id]
  end

  it "records a Token's first published balance" do
    expect(projector.apply(recorded(2, posted: -300, outgoing: 50, incoming: 20))).to be true

    expect(row.values).to include(
      wallet_uuid: wallet_id, currency: 'USD', posted_minor_units: -300,
      pending_outgoing_minor_units: 50, pending_incoming_minor_units: 20, last_sequence: 2
    )
  end

  it 'replaces it with a later balance' do
    projector.apply(recorded(2, posted: 0, incoming: 400))

    expect(projector.apply(recorded(3, posted: 400))).to be true
    expect(row.values).to include(posted_minor_units: 400, pending_incoming_minor_units: 0, last_sequence: 3)
  end

  it 'never lets an older balance overwrite a newer one' do
    projector.apply(recorded(3, posted: 400))

    expect(projector.apply(recorded(2, posted: 0, incoming: 400))).to be false
    expect(projector.apply(recorded(3, posted: 400))).to be false
    expect(row.values).to include(posted_minor_units: 400, pending_incoming_minor_units: 0, last_sequence: 3)
  end

  it 'acknowledges TokenMinted without recording a balance' do
    minted = Token::V1::TokenMinted.new(id: token_id, wallet_id: wallet_id)

    expect(projector.apply(envelope_for(minted, aggregate_id: token_id, sequence: 1))).to be false
    expect(row).to be_nil
  end

  it 'refuses a Token event it has never heard of' do
    unknown = Token::V1::MintRequest.new(id: token_id)

    expect { projector.apply(envelope_for(unknown, aggregate_id: token_id, sequence: 1)) }
      .to raise_error(Consumer::EventStateMap::UnmappedEvent)
  end
end
