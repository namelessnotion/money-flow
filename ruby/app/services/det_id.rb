# frozen_string_literal: true
# typed: strict

require 'digest'

module Services
  # Ruby's copy of go/internal/detid: a stable, valid uuid derived from a seed.
  #
  # A follow-on Transaction Ruby originates — an ACH clearing, say — takes its
  # id from the aggregate that triggered it, so sending it again is a no-op in
  # Go however often Ruby forgets it already did (ruby/docs/adr/0001, decision
  # 4). The spec pins this to ids Go itself produced: the two must never drift.
  module DetId
    # SHA-256 of the seed, first 16 bytes, stamped as a version 4, RFC 4122
    # variant uuid — exactly detid.New.
    sig { params(seed: String).returns(String) }
    def self.for(seed)
      bytes = Digest::SHA256.digest(seed).bytes.first(16)
      bytes[6] = (bytes.fetch(6) & 0x0f) | 0x40
      bytes[8] = (bytes.fetch(8) & 0x3f) | 0x80
      format_uuid(bytes.pack('C*').unpack1('H*').to_s)
    end

    # 32 hex digits, grouped 8-4-4-4-12.
    sig { params(hex: String).returns(String) }
    def self.format_uuid(hex)
      hex.unpack('a8a4a4a4a12').join('-')
    end
    private_class_method :format_uuid
  end
end
