# frozen_string_literal: true
# typed: strict

module Consumer
  # One message as read off a topic, before anything is made of its contents.
  class Message < T::Struct
    const :topic, String
    const :partition, Integer
    const :offset, Integer
    const :key, T.nilable(String)
    const :value, T.nilable(String)

    # Where the message sits, for diagnostics: topic[partition]@offset.
    sig { returns(String) }
    def position
      "#{topic}[#{partition}]@#{offset}"
    end
  end
end
