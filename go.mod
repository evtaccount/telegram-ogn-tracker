module telegram-ogn-tracker

go 1.23.8

require (
	github.com/go-telegram-bot-api/telegram-bot-api/v5 v5.5.1
	ogn v0.0.0-20250609115154-0c62c54f4be5
)

replace ogn => github.com/evtaccount/ogn-client v0.0.0-20250609115154-0c62c54f4be5
