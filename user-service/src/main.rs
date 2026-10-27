use sqlx::{PgPool, postgres::PgPoolOptions};
use std::time::Duration;
use tonic::{Request, Response, Status, transport::Server};
use tonic_health::ServingStatus;
use user::user_service_server::{UserService, UserServiceServer};
use user::{
    CreateProfileRequest, CreateProfileResponse, GetPublicProfileRequest, GetPublicProfileResponse,
};

use crate::error::database_status;

mod error;
pub mod user {
    tonic::include_proto!("user");
}

struct ProfileService {
    pg: PgPool,
}

fn valid_text(value: &str, max: usize) -> bool {
    !value.is_empty()
        && value.trim() == value
        && value.chars().count() <= max
        && !value.chars().any(char::is_control)
}

#[allow(clippy::result_large_err)] // tonic::Status is the RPC error type; boxing it here hides nothing.
fn parse_user_id(value: &str) -> Result<uuid::Uuid, Status> {
    uuid::Uuid::parse_str(value).map_err(|_| Status::invalid_argument("invalid user id"))
}

#[tonic::async_trait]
impl UserService for ProfileService {
    async fn create_profile(
        &self,
        request: Request<CreateProfileRequest>,
    ) -> Result<Response<CreateProfileResponse>, Status> {
        let req = request.into_inner();
        let user_id = parse_user_id(&req.user_id)?;
        if !valid_text(&req.name, 50) || !valid_text(&req.username, 255) {
            return Err(Status::invalid_argument("invalid profile fields"));
        }
        // Idempotent: Auth retries this after an unknown registration outcome.
        // A conflicting username for a different user_id surfaces as AlreadyExists.
        let result = sqlx::query(
            "INSERT INTO profiles (user_id, name, username) VALUES ($1, $2, $3)
             ON CONFLICT (user_id) DO UPDATE SET name = EXCLUDED.name, username = EXCLUDED.username
             WHERE profiles.username = EXCLUDED.username",
        )
        .bind(user_id)
        .bind(&req.name)
        .bind(&req.username)
        .execute(&self.pg)
        .await
        .map_err(database_status)?;
        if result.rows_affected() == 0 {
            return Err(Status::already_exists(
                "profile exists with a different username",
            ));
        }
        Ok(Response::new(CreateProfileResponse { status: 0 }))
    }

    // Public view: name and username only. No credentials or contact data
    // may ever appear here, so the response is safe for any caller.
    async fn get_public_profile(
        &self,
        request: Request<GetPublicProfileRequest>,
    ) -> Result<Response<GetPublicProfileResponse>, Status> {
        let req = request.into_inner();
        let user_id = parse_user_id(&req.user_id)?;
        let profile: Option<(String, String)> =
            sqlx::query_as("SELECT name, username FROM profiles WHERE user_id = $1")
                .bind(user_id)
                .fetch_optional(&self.pg)
                .await
                .map_err(database_status)?;
        match profile {
            Some((name, username)) => {
                Ok(Response::new(GetPublicProfileResponse { name, username }))
            }
            None => Err(Status::not_found("profile not found")),
        }
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt().json().with_target(false).init();
    let addr = std::env::var("GRPC_ADDR")
        .unwrap_or_else(|_| "127.0.0.1:50051".into())
        .parse()?;
    let database_url = std::env::var("DATABASE_URL")?;
    let pool = PgPoolOptions::new()
        .max_connections(10)
        .acquire_timeout(Duration::from_secs(2))
        .connect(&database_url)
        .await?;
    sqlx::migrate!().run(&pool).await?;
    let service = ProfileService { pg: pool.clone() };
    let (reporter, health_service) = tonic_health::server::health_reporter();
    reporter
        .set_serving::<UserServiceServer<ProfileService>>()
        .await;
    let monitor_pool = pool.clone();
    let monitor = tokio::spawn(async move {
        let mut interval = tokio::time::interval(Duration::from_secs(1));
        loop {
            interval.tick().await;
            let healthy = tokio::time::timeout(
                Duration::from_secs(1),
                sqlx::query("SELECT 1").execute(&monitor_pool),
            )
            .await
            .is_ok_and(|result| result.is_ok());
            let status = if healthy {
                ServingStatus::Serving
            } else {
                ServingStatus::NotServing
            };
            reporter.set_service_status("", status).await;
            reporter
                .set_service_status("user.UserService", status)
                .await;
        }
    });
    let mut terminate = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
    tracing::info!(address = %addr, "user service starting");
    let result = Server::builder()
        .timeout(Duration::from_secs(5))
        .add_service(health_service)
        .add_service(
            UserServiceServer::new(service)
                .max_decoding_message_size(8 << 10)
                .max_encoding_message_size(8 << 10),
        )
        .serve_with_shutdown(addr, async move {
            tokio::select! {
                result = tokio::signal::ctrl_c() => {
                    if result.is_err() {
                        tracing::error!("failed to listen for interrupt");
                    }
                }
                _ = terminate.recv() => {}
            }
        })
        .await;
    monitor.abort();
    pool.close().await;
    result?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validates_profile_fields() {
        assert!(valid_text("Worker", 50));
        assert!(!valid_text(&"я".repeat(51), 50));
        assert!(valid_text(&"я".repeat(50), 50));
        assert!(!valid_text(" padded", 50));
        assert!(!valid_text("con\0trol", 50));
    }

    #[test]
    fn parses_only_uuid_user_ids() {
        assert!(parse_user_id("019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e6f").is_ok());
        assert_eq!(
            parse_user_id("42").unwrap_err().code(),
            tonic::Code::InvalidArgument
        );
        assert_eq!(
            parse_user_id("").unwrap_err().code(),
            tonic::Code::InvalidArgument
        );
    }

    #[tokio::test]
    async fn rejects_invalid_rpc_input_before_database_access() {
        let service = ProfileService {
            pg: PgPoolOptions::new()
                .connect_lazy("postgres://unused:unused@127.0.0.1:1/unused")
                .unwrap(),
        };
        let error = service
            .create_profile(Request::new(CreateProfileRequest {
                user_id: "not-a-uuid".into(),
                name: "Worker".into(),
                username: "worker".into(),
            }))
            .await
            .unwrap_err();
        assert_eq!(error.code(), tonic::Code::InvalidArgument);
        let error = service
            .create_profile(Request::new(CreateProfileRequest {
                user_id: "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e6f".into(),
                name: " ".into(),
                username: "worker".into(),
            }))
            .await
            .unwrap_err();
        assert_eq!(error.code(), tonic::Code::InvalidArgument);
        let error = service
            .get_public_profile(Request::new(GetPublicProfileRequest {
                user_id: "not-a-uuid".into(),
            }))
            .await
            .unwrap_err();
        assert_eq!(error.code(), tonic::Code::InvalidArgument);
    }
}
