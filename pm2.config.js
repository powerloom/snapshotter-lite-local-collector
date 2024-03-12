// this means if app restart {MAX_RESTART} times in 1 min then it stops
// const MAX_RESTART = 10;
// const MIN_UPTIME = 60000;
const NODE_ENV = process.env.NODE_ENV || 'development';

module.exports = {
    apps : [
        {
            name   : "proto-snapshot-server",
            script : "./cmd/cmd",
            cwd : `${__dirname}/cmd`,
            // max_restarts: MAX_RESTART,
            // min_uptime: MIN_UPTIME,
            // kill_timeout : 3000,
            env: {
                NODE_ENV: NODE_ENV,
                CONFIG_PATH:`${__dirname}`
            },
            args: "5" //Log level set to debug, for production change to 4 (INFO) or 2(ERROR)
        }
    ]
}