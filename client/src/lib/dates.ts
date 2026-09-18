const dateTimeFormatter = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })
const dateFormatter = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeZone: 'UTC' })

export function formatDateTime(value: string): string {
  return dateTimeFormatter.format(new Date(value))
}

// An ISO 8601 date (no time): read as UTC so it is not shifted a day by the
// viewer's offset.
export function formatDate(value: string): string {
  return dateFormatter.format(new Date(`${value}T00:00:00Z`))
}
