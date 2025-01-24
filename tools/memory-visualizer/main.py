import argparse
import os
import pandas as pd
import plotly.graph_objects as go

# The CVS file should have the following columns:
# time, memory_usage, red_dot
# red_dot should be true or false
# The red_dot column is used to mark the points in the graph: those with red_dot=true should have
# red round markers, the others should have no markers

# Get file from the argument
def main():
    args = parse_args()
    if args.heap_file is None or args.rss_file is None:
        print("Please provide a heap file and a rss file")
        return
    # Check if file exists
    if not os.path.isfile(args.heap_file):
        print(f"Heap file '{args.file}' does not exist")
        return
    if not os.path.isfile(args.rss_file):
        print(f"RSS file '{args.file}' does not exist")
        return

    df_heap = pd.read_csv(args.heap_file)
    df_rss = pd.read_csv(args.rss_file)

    fig = go.Figure()

    # Add the main line with all the usage data
    fig.add_trace(go.Scatter(x=df_heap['time'], y=df_heap['memory_usage'],
                             mode='lines', name='Memory Usage'))

    # Add the red circles around the points with red_dot=true
    red_dot = df_heap[df_heap['red_dot'] == True]
    fig.add_trace(go.Scatter(
        x=red_dot['time'],
        y=red_dot['memory_usage'],
        mode='markers',
        marker=dict(color='red', symbol='circle', size=10),
        name='Detector Triggered'))

    # Add the main line with RSS data from the RSS file
    fig.add_trace(go.Scatter(
        x=df_rss['time'],
        y=df_rss['memory_usage'],
        mode='lines',
        name='RSS'))


    # Add buttons to switch between Bytes and Megabytes
    fig.update_layout(
        updatemenus=[
            dict(
                buttons=list([
                    dict(
                        args=[{'y': [df_heap['memory_usage'], red_dot['memory_usage'], df_rss['memory_usage']]}],
                        label='Bytes',
                        method='restyle'
                    ),
                    dict(
                        args=[{'y': [df_heap['memory_usage'] / 1024 / 1024,
                                     red_dot['memory_usage'] / 1024 / 1024,
                                     df_rss['memory_usage'] / 1024 / 1024],
                               }],
                        label='Megabytes',
                        method='restyle'
                    )
                ]),
                direction='down',
                showactive=True,
            ),
        ]
    )

    fig.show()

def parse_args():
    parser = argparse.ArgumentParser(description='Visualize memory usage')
    parser.add_argument('heap_file', help='Pillar heap file to visualize')
    parser.add_argument('rss_file', help='RSS file to visualize')
    return parser.parse_args()


if __name__ == '__main__':
    main()
