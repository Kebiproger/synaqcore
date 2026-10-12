``` go
        <-shutdownCtx.Done()
        log.Println("Получен сигнал завершения работы, останавливаем сервис...")
    
        // 7. Мягкая остановка HTTP-сервера (даем 5 секунд на завершение текущих
  запросов)
        stopHttpCtx, cancelHttp := context.WithTimeout(context.Background(), 5*time.
  Second)
        defer cancelHttp()
        if err := srv.Shutdown(stopHttpCtx); err != nil {
            log.Printf("Ошибка при остановке HTTP-сервера: %v", err)
        }
    
        // 8. Сброс буферов (Flush) и закрытие соединения с Kafka
        if err := producer.Close(); err != nil {
            log.Printf("Ошибка при закрытии Kafka продюсера: %v", err)
        } else {
            log.Println("Kafka продюсер успешно остановлен")
        }
    
        log.Println("Сервис успешно выключен")
    }
```