# frozen_string_literal: true

# Records every SQL statement Sequel logs while the block runs, so a spec can
# assert on exactly which queries (and columns) were issued without stubbing
# Sequel — the only real evidence that something is batched.
module SqlCapture
  def capture_sql
    statements = []
    recorder = Object.new
    %i[info warn error].each { |level| recorder.define_singleton_method(level) { |message| statements << message } }

    DB.loggers << recorder
    yield
    statements
  ensure
    DB.loggers.delete(recorder)
  end
end
