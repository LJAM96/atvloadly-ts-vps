use std::fs;
use std::path::PathBuf;

use anyhow::{Context, Result, anyhow};
use clap::Parser;
use idevice::{Idevice, lockdown::LockdownClient};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::net::TcpStream;
use uuid::Uuid;

#[derive(Debug, Parser)]
#[command(about = "Pair an Apple TV directly by IP and emit a classic lockdown record")]
struct Args {
    #[arg(long = "ip", value_name = "IP")]
    ip: String,

    #[arg(long = "lockdown-dir", value_name = "DIR", default_value = "/var/lib/lockdown")]
    lockdown_dir: PathBuf,
}

fn new_uuid() -> String {
    Uuid::new_v4().to_string().to_uppercase()
}

#[tokio::main]
async fn main() {
    if let Err(err) = run().await {
        println!("ERROR: {err}");
        std::process::exit(1);
    }
}

async fn run() -> Result<()> {
    let args = Args::parse();
    let socket = TcpStream::connect(format!("{}:{}", args.ip, LockdownClient::LOCKDOWND_PORT))
        .await
        .with_context(|| format!("failed to connect to {}:{}", args.ip, LockdownClient::LOCKDOWND_PORT))?;

    let idevice = Idevice::new(Box::new(socket), "atvloadly-direct-pair".to_string());
    let mut lockdown = LockdownClient::new(idevice);

    let host_id = new_uuid();
    let system_buid = new_uuid();

    println!("Starting wireless pairing...");
    let pin_callback = || async {
        println!("Enter PIN:");
        let mut lines = BufReader::new(tokio::io::stdin()).lines();
        lines
            .next_line()
            .await
            .unwrap_or_default()
            .unwrap_or_default()
    };

    lockdown
        .cu_pairing_create(system_buid.clone(), pin_callback, None)
        .await
        .context("failed to perform wireless pairing handshake")?;

    let mut pairing_file = lockdown
        .pair_cu(host_id, system_buid)
        .await
        .context("failed to create pairing record")?;

    lockdown
        .start_session(&pairing_file)
        .await
        .context("pairing file validation failed")?;

    let udid = lockdown
        .get_value(Some("UniqueDeviceID"), None)
        .await
        .context("failed to read UniqueDeviceID")?
        .as_string()
        .map(str::to_string)
        .ok_or_else(|| anyhow!("UniqueDeviceID missing from lockdown response"))?;

    if pairing_file.udid.is_none() {
        pairing_file.udid = Some(udid.clone());
    }

    fs::create_dir_all(&args.lockdown_dir)
        .with_context(|| format!("failed to create {}", args.lockdown_dir.display()))?;
    let output_path = args.lockdown_dir.join(format!("{udid}.plist"));
    let bytes = pairing_file.serialize().context("failed to serialize pairing record")?;
    fs::write(&output_path, bytes)
        .with_context(|| format!("failed to write {}", output_path.display()))?;

    println!("SUCCESS: UDID `{udid}`");
    println!("Saved pairing file to {}", output_path.display());
    Ok(())
}
