# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::DetId do
  # Produced by go/internal/detid.New itself: Ruby and Go must derive the same
  # id from the same seed, or a follow-on Transaction Ruby originates would not
  # be the one Go already has.
  it 'derives the same id as Go for the same seed' do
    expect(described_class.for('seed-1')).to eq('0eb02673-1d9e-43f8-b051-1f8c18daeb81')
    expect(described_class.for('01a0b4e1-7da9-7319-bfbb-a9c1417d4129:clearing'))
      .to eq('0c2e7cc9-5c84-4b3b-9783-d68af3256ea1')
  end

  it 'is deterministic, and distinct seeds give distinct ids' do
    first = described_class.for('a')
    expect(described_class.for('a')).to eq(first)
    expect(described_class.for('a')).not_to eq(described_class.for('b'))
  end

  it 'is a version 4, RFC 4122 variant uuid' do
    expect(described_class.for('seed-1')).to match(/\A\h{8}-\h{4}-4\h{3}-[89ab]\h{3}-\h{12}\z/)
  end
end
