-- Уведомление об изменениях experiments через LISTEN/NOTIFY (канал experiments_changed):
-- инстансы synaq перечитывают кэш конфигураций без опроса БД.
-- Payload: {"op": "INSERT"|"UPDATE"|"DELETE", "name": "<эксперимент>"}.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION experiments_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify(
        'experiments_changed',
        json_build_object('op', TG_OP, 'name', COALESCE(NEW.name, OLD.name))::text
    );
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS experiments_notify ON experiments;

CREATE TRIGGER experiments_notify
AFTER INSERT OR UPDATE OR DELETE ON experiments
FOR EACH ROW EXECUTE FUNCTION experiments_notify();

-- +goose Down
DROP TRIGGER IF EXISTS experiments_notify ON experiments;
DROP FUNCTION IF EXISTS experiments_notify();
