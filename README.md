# agy-api

Локальный Go-прокси к Google Antigravity с OpenAI-совместимыми `GET /v1/models` и `POST /v1/chat/completions`. Для текстового чата можно передать историю и включить streaming через `"stream": true`. Tools, изображения и Responses API пока не поддерживаются.

## Первый запуск

1. Подготовьте OAuth-клиент типа **Desktop app**, которому Google предоставил доступ к внутреннему API Antigravity. Клиент и аккаунт должны иметь доступ к нужным scopes. Обычная регистрация OAuth-клиента в Google Cloud позволяет начать вход, но доступ к внутреннему API предоставляется отдельно. Если приложение в режиме Testing, добавьте свой аккаунт в список тестовых пользователей.
2. Скопируйте `.env.example` в `.env` и заполните `ANTIGRAVITY_OAUTH_CLIENT_ID` и `ANTIGRAVITY_OAUTH_CLIENT_SECRET` из настроек клиента.
3. Запустите `go run .` из каталога проекта (Go 1.25+) или запустите готовый бинарник из каталога с `.env`. Готовые бинарники для Linux, macOS и Windows лежат в артефакте `agy-api-binaries` успешной сборки на вкладке [Actions](https://github.com/skb8/agy-api/actions).
4. Откройте напечатанную ссылку в браузере на той же машине и войдите в Google. Прокси сохранит access token, refresh token и срок действия в `.env`. Первый запуск генерирует локальный API-ключ. Следующие запуски выводят тот же ключ и обновляют токены по мере необходимости. Для повторного входа используйте `go run . --login` или запустите бинарник с `--login`.

После входа прокси выводит базовый URL и список моделей, полученный у Antigravity для вашего аккаунта. Если загрузка списка не удалась, сервер продолжает работать, а запрос к `/v1/models` повторяет её. Опционально задайте `AGY_LISTEN` для смены адреса и `ANTIGRAVITY_PROJECT_ID`, если нужно указать ID проекта вручную. Держите один экземпляр прокси на каталог с `.env`.

## Запросы

В OpenAI-клиенте укажите Base URL `http://127.0.0.1:8080/v1` и ключ `agy_…` из терминала.

```bash
export AGY_API_KEY='agy_...'
curl http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer $AGY_API_KEY"

curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $AGY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"ID_ИЗ_СПИСКА","messages":[{"role":"user","content":"Привет!"}]}'
```

Прокси использует внутренний интерфейс Antigravity `v1internal`. Доступ к нему и моделям зависит от аккаунта, OAuth-клиента и изменений на стороне Google. Схема запросов сверена с [AIClient2API](https://github.com/justlovemaki/AIClient2API) и [OmniRoute](https://github.com/ChrisCompton/omniroute).

### HTTP 403 с причиной `SERVICE_DISABLED`

В терминале ищите `service=` и `consumer=projects/…`. Эти поля идентифицируют сервис и проект, для которого Google проверил доступ. Если проект ваш и нужный сервис доступен вам в API Library, включите его в этом проекте. Для `cloudcode-pa.googleapis.com` может потребоваться отдельный доступ от Google. Доступ к Cloud Code Private API настраивается отдельно от публичного Gemini API и `ANTIGRAVITY_PROJECT_ID`.
