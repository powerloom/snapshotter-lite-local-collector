
const NODE_ENV = process.env.NODE_ENV || 'development';
const ENABLE_CRON_RESTART = process.env.ENABLE_CRON_RESTART_LOCAL_COLLECTOR === 'true';
console.log("ENABLE_CRON_RESTART flag value: ", ENABLE_CRON_RESTART);
module.exports = {
    apps : [
        {
            name   : "snapshot-local-collector",
            script : "./cmd/cmd",
            cwd : `${__dirname}/cmd`,
            env: {
                NODE_ENV: NODE_ENV,
                CONFIG_PATH:`${__dirname}`
            },
            args: "5", //Log level set to debug, for production change to 4 (INFO) or 2(ERROR)
            cron_restart: ENABLE_CRON_RESTART ? "0 * * * *" : false // Restart every hour
        }
    ]
}
