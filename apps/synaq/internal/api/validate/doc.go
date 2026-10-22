// Package validate разбирает и проверяет тела запросов API экспериментов по тем
// же правилам, что и pydantic-схемы Python-версии experiment_api (тег v1-python).
//
// Совместимость с FastAPI:
//   - коды ответа совпадают: 422 для ошибок валидации, 400 для тела, которое
//     не декодируется как UTF-8 ("There was an error parsing the body");
//   - формат 422: {"detail":[{"type","loc","msg"[,"input"]}]}, loc и type как у pydantic,
//     русские сообщения value_error (имя, зарезервированное имя, веса) совпадают дословно;
//   - пустое тело или JSON null дают missing в ["body"], битый JSON — json_invalid
//     в ["body", <смещение в байтах>].
//
// Сознательные отличия от pydantic:
//   - типы строгие, без lax-приведения: вес "5", 5.0, true и is_active "yes", 1 — ошибка
//     (int_type / bool_type / string_type с текстом pydantic strict-режима);
//   - для нецелого числа в весе всегда int_type (pydantic в lax-режиме дал бы int_from_float);
//   - порядок ошибок — порядок объявления полей модели, ошибки элементов — в порядке
//     ключей во входном JSON; ctx не выводится, input — исходный JSON значения
//     (для missing и ошибок уровня модели input не выводится);
//   - NaN/Infinity — невалидный JSON (Python json.loads их принимает).
//
// Размер тела ограничивает вызывающий код (http.MaxBytesReader и т. п.).
package validate
